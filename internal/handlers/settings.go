package handlers

import (
	"net/http"

	"weight-tracker/internal/db"
)

func (s *Server) HandleDeleteAll(w http.ResponseWriter, r *http.Request) {
	if err := db.DeleteAllData(r.Context(), s.db); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
