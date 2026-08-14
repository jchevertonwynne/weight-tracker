package main

import (
	"net/http"

	"weight-tracker/internal/db"
)

func (s *server) handleDeleteAll(w http.ResponseWriter, r *http.Request) {
	if err := db.DeleteAllData(s.db); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
