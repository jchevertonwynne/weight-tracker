package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	// Embeds the IANA timezone database in the binary, used only when the
	// host has none of its own.
	//
	// This app reads and writes local wall-clock times and splits weigh-ins
	// into morning and evening on them, so time.Local has to be the real
	// zone. On the Pi that came from /etc/localtime. In a FROM scratch
	// container there is no /usr/share/zoneinfo at all, and Go silently
	// falls back to UTC — the app would keep working and quietly file 00:30
	// BST entries against the previous day. Embedding costs ~450KB and
	// removes the dependency on the image having zone data.
	//
	// The container must still set TZ (see the deployment manifest);
	// embedding provides the data, TZ chooses which zone.
	_ "time/tzdata"

	"github.com/jchevertonwynne/homelab-go/access"
	"github.com/jchevertonwynne/homelab-go/logging"
	"github.com/jchevertonwynne/homelab-go/metrics"
	"github.com/jchevertonwynne/homelab-go/profiling"
	"github.com/jchevertonwynne/homelab-go/serve"
	"github.com/jchevertonwynne/homelab-go/tracing"

	"weight-tracker/internal/db"
	"weight-tracker/internal/handlers"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

func main() {
	logging.Init()
	if err := run(); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// run exists so that the deferred shutdowns below actually happen.
// os.Exit skips every pending defer, so keeping the one exiting path in main
// and everything else here means a failure to open the database no longer
// discards the tracing flush on its way out.
func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "weight-tracker.db", "path to sqlite database file")
	otelEndpoint := flag.String("otel-endpoint", "", "host:port of an OTLP/gRPC trace collector; tracing is disabled if empty")
	pprofAddr := flag.String("pprof-addr", ":6060", "listen address for pprof debug endpoints; never expose this outside the cluster")
	devUser := flag.String("dev-user", "", "email to assume when no Access header is present; local development only")
	allowedEmails := flag.String("allowed-emails", "", "comma-separated addresses this hostname's Cloudflare Access policy admits; anyone else is refused even with a live Access session")
	flag.Parse()

	allow := access.NewAllowlist(*allowedEmails, *devUser)
	if *devUser != "" {
		// The address is deliberately not logged: it is the identity every
		// header-less request is about to be treated as, and these lines go to
		// Loki, where an email address is a worse thing to have than a line
		// telling you to go and read the flags.
		slog.Warn("-dev-user is set; every request without an Access header is treated as that user")
	}
	if !allow.Configured() {
		// Not fatal yet. This app is being given its allowlist in two steps —
		// the flag first, then the ConfigMap that fills it — because a pod
		// whose image does not yet know a flag refuses to start at all.
		slog.Warn("-allowed-emails is empty; anyone Cloudflare Access admits can use this app, including a session issued before someone was removed from the policy")
	}

	go profiling.ListenAndServe(*pprofAddr)

	// This used to be best-effort only: the app ignored SIGTERM, so on a pod
	// delete the deferred shutdown never ran and the last batch of spans was
	// lost. The signal handling further down means it runs now, so whatever
	// was still buffered when a rollout started reaches Alloy.
	shutdownTracing, err := tracing.Init(context.Background(), "weight-tracker", *otelEndpoint)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			slog.Error("shutdown tracing", "error", err)
		}
	}()

	sqlDB, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()

	app := handlers.New(sqlDB, templatesFS, staticFS, time.Now)
	mux, appMux := routes(app, allow)

	journalMode, err := db.JournalMode(sqlDB)
	if err != nil {
		return fmt.Errorf("read journal mode: %w", err)
	}
	if journalMode != "wal" {
		// Not fatal — the rollback journal is still correct — but worth
		// saying out loud, since it usually means the database lives on a
		// filesystem that cannot do WAL.
		slog.Warn("journal mode is not WAL", "journal_mode", journalMode)
	}

	// refuseCrossOrigin outside Instrument, so Instrument still measures every
	// request. appMux is handed to it as well, because the application routes
	// now sit behind the outer mux's "/" catch-all and would otherwise all be
	// labelled with that one pattern.
	if err := serve.Run(*addr, tracing.Middleware("weight-tracker", refuseCrossOrigin(metrics.Instrument(mux, metrics.WithRouters(appMux)))),
		serve.WithLogAttrs("db", *dbPath, "journal_mode", journalMode)); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// routes builds the mux, with every application route behind authenticate.
//
// The split is the point. Application routes are registered on their own mux
// and reached through the "/" catch-all, so a route added to RegisterRoutes
// later is authenticated without anyone remembering to make it so. The
// exceptions are named one by one here, each for a reason:
//
//   - /healthz is probed by the kubelet, which arrives as the node with no
//     Access session, and a probe that depends on Access would restart the pod
//     whenever Access broke rather than when the app did.
//   - /metrics is scraped by Alloy, straight at the pod IP, likewise.
//   - /static/ and /sw.js are the same bytes for everyone and carry nothing
//     about anybody.
//   - /backup.db is fetched hourly by the in-cluster backup CronJob, which has
//     no session either — and by the Download backup link on the settings
//     page, which does. See allowBackup.
//
// It returns the application mux alongside the one to serve, which is only
// for metrics labelling — see the call site.
func routes(app *handlers.Server, allow access.Allowlist) (http.Handler, *http.ServeMux) {
	appMux := http.NewServeMux()
	app.RegisterRoutes(appMux)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	for _, pattern := range []string{"GET /healthz", "GET /static/", "GET /sw.js"} {
		mux.Handle(pattern, appMux)
	}
	mux.Handle("GET /backup.db", allowBackup(allow, appMux))
	mux.Handle("/", authenticate(allow, appMux))
	return mux, appMux
}

// authenticate resolves the caller or refuses the request.
//
// This app stores one person's weigh-ins and has no login, no accounts and no
// per-user data: everyone who gets in sees and edits the same entries. So the
// only question it asks is whether the caller is on this hostname's Access
// allowlist, which is also why that list is the whole of its authorisation
// model.
//
// Refusing a request with no header at all matters as much as refusing an
// unlisted one. If the Access application in front of this hostname were
// removed, misconfigured or bypassed, the header would simply stop arriving,
// and treating that as "no identity required" would put a year of weigh-ins on
// the public internet. The NetworkPolicy is what makes the header itself
// trustworthy: apps/weight-tracker/networkpolicy.yaml in the homelab repo
// admits the cloudflared pod and the backup job, so nothing else in the
// cluster can set it to anything it likes.
func authenticate(allow access.Allowlist, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every page here is one person's weight history. None of it may be
		// served from a shared cache, Cloudflare's edge included.
		w.Header().Set("Cache-Control", "no-store")

		email, ok := access.Email(r, allow.DevUser())
		if !ok {
			slog.ErrorContext(r.Context(), "no Access header and no -dev-user configured", "header", access.EmailHeader)
			http.Error(w, "not authenticated", http.StatusForbidden)
			return
		}
		// Access checks its policy when it issues a session and not again, so
		// someone taken off the policy keeps a working header until that
		// session expires — a month, at this hostname's session length. This
		// is what ends it, once the pod has restarted onto the new ConfigMap.
		if !allow.Admits(email) {
			// The address is not logged, for the reason given on -dev-user
			// above. That a refusal happened is the part worth having.
			slog.WarnContext(r.Context(), "refusing a caller who is not on this hostname's allowlist")
			http.Error(w, "not on this app's allowlist", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowBackup guards /backup.db, which has two legitimate callers with
// nothing in common.
//
// The hourly CronJob reaches the Service directly from inside the cluster, so
// it has no Access session and sends no header; refusing it would end the
// backups. The Download backup link on the settings page comes through
// cloudflared, so it always carries one. A header that names someone off the
// allowlist is therefore a person whose session outlived their removal, and
// this file is every weigh-in in one download.
func allowBackup(allow access.Allowlist, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if email := access.NormalizeEmail(r.Header.Get(access.EmailHeader)); email != "" && !allow.Admits(email) {
			slog.WarnContext(r.Context(), "refusing a backup for a caller who is not on this hostname's allowlist")
			http.Error(w, "not on this app's allowlist", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseCrossOrigin refuses any state-changing request that another site made
// the browser send.
//
// This app has no login of its own. Cloudflare Access decides who is calling
// from its CF_Authorization cookie, and the browser attaches that cookie to
// any request to this hostname, including a form on another site that posts
// here. The Access application leaves SameSite unset, and Safari and Firefox
// then send the cookie on a cross-site POST. Without this, a page visited while
// signed in could auto-submit a form to POST /settings/delete-all and wipe
// every entry, or to POST /import and fill the chart with its own data.
//
// net/http's CrossOriginProtection refuses exactly that: a request other than
// GET, HEAD or OPTIONS whose Sec-Fetch-Site says another site sent it, or,
// from a browser too old to send that header, whose Origin names another
// host. The app's own htmx requests and forms are same-origin and pass, and a
// request with neither header is not a browser and so has no cookie to
// borrow. GET is never refused, which is why no GET here may change anything.
func refuseCrossOrigin(next http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.WarnContext(r.Context(), "refusing a cross-origin request", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	}))
	return p.Handler(next)
}
