// Package markers holds the display-ready Row form of a db.Marker and the
// Point projection used by the chart's marker-line overlay.
package markers

import (
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/timerange"
)

// Row is the display-ready form of a db.Marker for the marker-history list.
type Row struct {
	ID        int64
	DateLabel string
	DateInput string
	Note      string
}

// BuildRows assumes markers is newest-first (as returned by db.ListMarkers).
func BuildRows(markers []db.Marker) []Row {
	rows := make([]Row, len(markers))
	for i, m := range markers {
		rows[i] = Row{
			ID:        m.ID,
			DateLabel: m.Date.Format("Jan 2, 2006"),
			DateInput: m.Date.Format("2006-01-02"),
			Note:      m.Note,
		}
	}
	return rows
}

// Point is a marker clipped to the chart's visible x-range, ready for JSON
// serialization. ID is included so the client can pick a stable color per
// marker (stable across range changes, unlike a position-based index).
type Point struct {
	ID   int64  `json:"id"`
	X    int64  `json:"x"`
	Date string `json:"date"`
	Note string `json:"note"`
}

// Visible filters markers to those falling within the calendar days spanned
// by [from, until] and maps them to chart-ready points. Markers are
// date-only, so comparing against whole calendar days (rather than the
// exact instants of the first/last visible entries) avoids excluding a
// marker set for the same day as a visible entry just because it's dated
// earlier in that day than the entry's own timestamp.
func Visible(markers []db.Marker, from, until time.Time) []Point {
	startOfDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	endOfDay := time.Date(until.Year(), until.Month(), until.Day(), 23, 59, 59, 999999999, until.Location())

	var out []Point
	for _, m := range markers {
		if m.Date.Before(startOfDay) || m.Date.After(endOfDay) {
			continue
		}
		out = append(out, Point{
			ID:   m.ID,
			X:    timerange.MsOf(m.Date),
			Date: timerange.DateLabel(m.Date),
			Note: m.Note,
		})
	}
	return out
}
