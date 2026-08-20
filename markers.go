package main

import (
	"net/http"
	"strings"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/markers"
)

// renderMarkersList re-renders the markers-list card and fires
// markers-changed so the chart controls refresh themselves too.
func (s *server) renderMarkersList(w http.ResponseWriter) {
	markerList, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "markers-changed")
	data := struct{ Markers []markers.Row }{Markers: markers.BuildRows(markerList)}
	if err := tmpl.ExecuteTemplate(w, "markers-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleMarkerCreate(w http.ResponseWriter, r *http.Request) {
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
	if _, err := db.CreateMarker(s.db, date, note, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderMarkersList(w)
}

func (s *server) handleMarkerEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	marker, err := db.GetMarker(s.db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rows := markers.BuildRows([]db.Marker{marker})
	if err := tmpl.ExecuteTemplate(w, "marker-row-edit", rows[0]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleMarkerCancelEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	markerList, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range markers.BuildRows(markerList) {
		if row.ID == id {
			if err := tmpl.ExecuteTemplate(w, "marker-row", row); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *server) handleMarkerUpdate(w http.ResponseWriter, r *http.Request) {
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
	if err := db.UpdateMarker(s.db, id, date, note); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderMarkersList(w)
}

func (s *server) handleMarkerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteMarker(s.db, id); err != nil {
		writeDeleteError(w, err)
		return
	}
	s.renderMarkersList(w)
}
