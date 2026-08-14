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
}

// buildMarkerRows assumes markers is newest-first (as returned by
// db.ListMarkers).
func buildMarkerRows(markers []db.Marker) []MarkerRow {
	rows := make([]MarkerRow, len(markers))
	for i, m := range markers {
		rows[i] = MarkerRow{
			ID:        m.ID,
			DateLabel: m.Date.Format("Jan 2, 2006"),
			DateInput: m.Date.Format("2006-01-02"),
			Note:      m.Note,
		}
	}
	return rows
}

// MarkerPoint is a marker clipped to the chart's visible x-range, ready for
// JSON serialization.
type MarkerPoint struct {
	X    int64  `json:"x"`
	Date string `json:"date"`
	Note string `json:"note"`
}

// visibleMarkers filters markers to those falling within the calendar days
// spanned by [from, until] and maps them to chart-ready points. Markers are
// date-only, so comparing against whole calendar days (rather than the
// exact instants of the first/last visible entries) avoids excluding a
// marker set for the same day as a visible entry just because it's dated
// earlier in that day than the entry's own timestamp.
func visibleMarkers(markers []db.Marker, from, until time.Time) []MarkerPoint {
	startOfDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	endOfDay := time.Date(until.Year(), until.Month(), until.Day(), 23, 59, 59, 999999999, until.Location())

	var out []MarkerPoint
	for _, m := range markers {
		if m.Date.Before(startOfDay) || m.Date.After(endOfDay) {
			continue
		}
		out = append(out, MarkerPoint{
			X:    msOf(m.Date),
			Date: formatDateLabel(m.Date),
			Note: m.Note,
		})
	}
	return out
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderMarkersList(w)
}
