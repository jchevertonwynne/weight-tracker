package main

import (
	"net/http"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/summary"
)

func (s *server) handleSummary(w http.ResponseWriter, _ *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := summary.Build(entries, time.Now())
	if err := tmpl.ExecuteTemplate(w, "summary", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
