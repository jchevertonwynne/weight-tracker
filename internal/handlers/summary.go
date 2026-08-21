package handlers

import (
	"net/http"

	"weight-tracker/internal/db"
	"weight-tracker/internal/summary"
)

func (s *Server) HandleSummary(w http.ResponseWriter, _ *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := summary.Build(entries, s.now())
	if err := s.tmpl.ExecuteTemplate(w, "summary", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
