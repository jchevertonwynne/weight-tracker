package main

import (
	"context"
	"embed"
	"flag"
	"log"
	"net/http"
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
	"weight-tracker/internal/metrics"
	"weight-tracker/internal/tracing"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "weight-tracker.db", "path to sqlite database file")
	otelEndpoint := flag.String("otel-endpoint", "", "host:port of an OTLP/gRPC trace collector; tracing is disabled if empty")
	flag.Parse()

	// Best-effort: this app doesn't handle SIGTERM (see the ListenAndServe
	// call below), so on a pod delete this shutdown func never actually
	// runs and the last batch of spans is lost. Fine for a hobby app's
	// traffic volume; the exporter still flushes on its own timer.
	shutdownTracing, err := tracing.Init(context.Background(), "weight-tracker", *otelEndpoint)
	if err != nil {
		log.Fatalf("init tracing: %v", err)
	}
	defer shutdownTracing(context.Background())

	sqlDB, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	srv := handlers.New(sqlDB, templatesFS, staticFS, time.Now)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	mux.Handle("GET /metrics", metrics.Handler())

	journalMode, err := db.JournalMode(sqlDB)
	if err != nil {
		log.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		// Not fatal — the rollback journal is still correct — but worth
		// saying out loud, since it usually means the database lives on a
		// filesystem that cannot do WAL.
		log.Printf("warning: journal mode is %q, not WAL", journalMode)
	}

	log.Printf("weight-tracker listening on %s (db: %s, journal: %s)", *addr, *dbPath, journalMode)
	handler := tracing.Middleware("weight-tracker", metrics.Instrument(mux))
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatal(err)
	}
}
