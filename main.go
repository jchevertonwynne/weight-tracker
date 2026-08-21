package main

import (
	"embed"
	"flag"
	"log"
	"net/http"
	"time"

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
