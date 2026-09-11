package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

	"weight-tracker/internal/db"
	"weight-tracker/internal/handlers"
	"weight-tracker/internal/logging"
	"weight-tracker/internal/metrics"
	"weight-tracker/internal/profiling"
	"weight-tracker/internal/tracing"
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

	srv := &http.Server{
		Addr:    *addr,
		Handler: tracing.Middleware("weight-tracker", metrics.Instrument(mux)),
		// A service reachable from the internet needs these. Without
		// ReadHeaderTimeout a single client can hold a connection open
		// indefinitely by dribbling out headers.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds
	// before SIGKILL. Anything that must be flushed on the way out — here
	// the tracing shutdown deferred above — happens after Shutdown returns.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Buffered: a listener that fails after the signal has already been
	// caught has nobody left reading this channel, and an unbuffered send
	// would then block this goroutine forever.
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", *addr, "db", *dbPath, "journal_mode", journalMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
