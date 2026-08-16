package main

import (
	"net/http"
	"strings"
	"time"

	"weight-tracker/internal/db"
)

// MarkerRow is the display-ready form of a db.Marker for the marker-history
// list.
type MarkerRow struct {
	ID        int64
	DateLabel string
	DateInput string
	Note      string
	// DateMs is the marker's date as Unix milliseconds, so the graph's
	// marker legend can hide the ones outside the visible time range —
	// listing markers the chart isn't showing is what made the vertical
	// lines hard to match to their labels.
	DateMs int64
	// ColorIndex picks one of four label colours, matching the colour
	// Grafana draws this marker's line in. Grafana has no way to print an
	// annotation's note on the plot, so matching colours is what lets a
	// line be tied to its label without hovering. Must stay in step with
	// v_markers.color_index.
	ColorIndex int64
}

// buildMarkerRows assumes markers is newest-first (as returned by
// db.ListMarkers).
func buildMarkerRows(markers []db.Marker) []MarkerRow {
	rows := make([]MarkerRow, len(markers))
	for i, m := range markers {
		rows[i] = MarkerRow{
			ID:         m.ID,
			DateLabel:  m.Date.Format("Jan 2, 2006"),
			DateInput:  m.Date.Format("2006-01-02"),
			Note:       m.Note,
			DateMs:     m.Date.UnixMilli(),
			ColorIndex: m.ID % 4,
		}
	}
	return rows
}

// renderMarkersList re-renders the markers-list card and fires
// markers-changed so the chart controls refresh themselves too.
func (s *server) renderMarkersList(w http.ResponseWriter) {
	markers, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "markers-changed")
	data := struct{ Markers []MarkerRow }{Markers: buildMarkerRows(markers)}
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
	rows := buildMarkerRows([]db.Marker{marker})
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
	markers, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range buildMarkerRows(markers) {
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
