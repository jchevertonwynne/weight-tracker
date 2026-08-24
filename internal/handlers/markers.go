package handlers

import (
	"context"
	"net/http"
	"strings"

	"weight-tracker/internal/db"
	"weight-tracker/internal/markers"
)

// RenderMarkersList re-renders the markers-list card and fires
// markers-changed so the chart controls refresh themselves too.
func (s *Server) RenderMarkersList(ctx context.Context, w http.ResponseWriter) {
	markerList, err := db.ListMarkers(ctx, s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "markers-changed")
	data := struct{ Markers []markers.Row }{Markers: markers.BuildRows(markerList)}
	if err := s.tmpl.ExecuteTemplate(w, "markers-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) HandleMarkerCreate(w http.ResponseWriter, r *http.Request) {
	date, err := parseDateField(r, "date")
	if err != nil {
		http.Error(w, "invalid date", http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if note == "" {
		http.Error(w, "note is required", http.StatusBadRequest)
		return
	}
	if _, err := db.CreateMarker(r.Context(), s.db, date, note, s.now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.RenderMarkersList(r.Context(), w)
}

func (s *Server) HandleMarkerEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	marker, err := db.GetMarker(r.Context(), s.db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rows := markers.BuildRows([]db.Marker{marker})
	if err := s.tmpl.ExecuteTemplate(w, "marker-row-edit", rows[0]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) HandleMarkerCancelEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	markerList, err := db.ListMarkers(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range markers.BuildRows(markerList) {
		if row.ID == id {
			if err := s.tmpl.ExecuteTemplate(w, "marker-row", row); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) HandleMarkerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	date, err := parseDateField(r, "date")
	if err != nil {
		http.Error(w, "invalid date", http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if note == "" {
		http.Error(w, "note is required", http.StatusBadRequest)
		return
	}
	if err := db.UpdateMarker(r.Context(), s.db, id, date, note); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.RenderMarkersList(r.Context(), w)
}

func (s *Server) HandleMarkerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteMarker(r.Context(), s.db, id); err != nil {
		writeDeleteError(w, err)
		return
	}
	s.RenderMarkersList(r.Context(), w)
}
