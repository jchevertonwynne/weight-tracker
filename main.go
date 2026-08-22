package main

import (
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
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "weight-tracker.db", "path to sqlite database file")
	flag.Parse()

	sqlDB, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	srv := handlers.New(sqlDB, templatesFS, staticFS, time.Now)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

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
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
