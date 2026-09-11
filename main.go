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
	flag.Parse()

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
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)
	mux.Handle("GET /metrics", metrics.Handler())

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

	// refuseCrossOrigin outside Instrument, so Instrument keeps seeing the
	// mux itself and labelling requests with its real routes.
	if err := serve.Run(*addr, tracing.Middleware("weight-tracker", refuseCrossOrigin(metrics.Instrument(mux))),
		serve.WithLogAttrs("db", *dbPath, "journal_mode", journalMode)); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
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
