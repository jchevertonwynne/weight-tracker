package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"weight-tracker/internal/db"
)

// Row is the display-ready form of a db.Entry. Period is derived from
// RecordedAt at render time rather than stored, per db.DetectPeriod.
type Row struct {
	ID              int64
	RecordedAtLabel string
	RecordedAtDate  string // for the edit form's date input
	RecordedAtTime  string // for the edit form's time input
	Period          string
	PeriodLabel     string
	WeightKgRaw     string
	WeightKgStr     string
	OvernightDelta  string // set on morning entries: vs. the prior evening
	OvernightLoss   bool
	DailyDelta      string // set on evening entries: vs. that same day's morning
	DailyGain       bool
}

// chronologicalWithDeltas orders entries by RecordedAt and, keyed by entry
// ID, computes two deltas:
//   - overnight: a morning entry's weight vs. the most recent evening entry
//     seen so far, but only if that evening entry was actually the
//     preceding calendar day — otherwise there's a gap (e.g. no evening
//     logged in between) and the two readings aren't really "overnight"
//     from each other, so no delta is shown.
//   - daily: an evening entry's weight vs. the most recent morning entry
//     seen so far on that SAME calendar day, i.e. same-day weight gained.
func chronologicalWithDeltas(entries []db.Entry) (chrono []db.Entry, overnight, daily map[int64]float64) {
	chrono = make([]db.Entry, len(entries))
	copy(chrono, entries)
	sort.SliceStable(chrono, func(i, j int) bool {
		return chrono[i].RecordedAt.Before(chrono[j].RecordedAt)
	})

	overnight = make(map[int64]float64, len(entries))
	daily = make(map[int64]float64, len(entries))
	var lastMorning, lastEvening *db.Entry
	for i := range chrono {
		e := &chrono[i]
		switch db.DetectPeriod(e.RecordedAt) {
		case "morning":
			if lastEvening != nil && isNextCalendarDay(lastEvening.RecordedAt, e.RecordedAt) {
				overnight[e.ID] = e.WeightKg - lastEvening.WeightKg
			}
			lastMorning = e
		case "evening":
			if lastMorning != nil && sameDate(lastMorning.RecordedAt, e.RecordedAt) {
				daily[e.ID] = e.WeightKg - lastMorning.WeightKg
			}
			lastEvening = e
		}
	}
	return chrono, overnight, daily
}

func sameDate(a, b time.Time) bool {
	return a.Format("2006-01-02") == b.Format("2006-01-02")
}

// isNextCalendarDay reports whether b falls on the calendar day immediately
// after a — used to require a genuine "yesterday evening to this morning"
// adjacency for the overnight delta, rather than comparing against
// whatever evening entry happens to be most recent regardless of gap size.
func isNextCalendarDay(a, b time.Time) bool {
	return a.AddDate(0, 0, 1).Format("2006-01-02") == b.Format("2006-01-02")
}

func buildRows(entries []db.Entry) []Row {
	_, overnightByID, dailyByID := chronologicalWithDeltas(entries)

	rows := make([]Row, len(entries))
	for i, e := range entries {
		period := db.DetectPeriod(e.RecordedAt)
		r := Row{
			ID:              e.ID,
			RecordedAtLabel: e.RecordedAt.Format("Jan 2, 2006 15:04"),
			RecordedAtDate:  e.RecordedAt.Format("2006-01-02"),
			RecordedAtTime:  e.RecordedAt.Format("15:04"),
			Period:          period,
			WeightKgRaw:     fmt.Sprintf("%g", e.WeightKg),
			WeightKgStr:     fmt.Sprintf("%.1f", e.WeightKg),
		}
		if period == "morning" {
			r.PeriodLabel = "Morning"
		} else {
			r.PeriodLabel = "Evening"
		}
		if delta, ok := overnightByID[e.ID]; ok {
			r.OvernightLoss = delta < 0
			r.OvernightDelta = fmt.Sprintf("%+.1f kg", delta)
		}
		if delta, ok := dailyByID[e.ID]; ok {
			r.DailyGain = delta > 0
			r.DailyDelta = fmt.Sprintf("%+.1f kg", delta)
		}
		rows[i] = r
	}
	return rows
}

// parseRecordedAt reads the split recorded_at_date/recorded_at_time fields.
func parseRecordedAt(r *http.Request) (time.Time, error) {
	return parseDateTimeFields(r, "recorded_at")
}

// renderEntriesList re-renders the history list after a create/update/delete
// and asks the chart controls to refresh themselves too, since a changed
// entry can affect whatever range/series the user currently has selected.
func (s *server) renderEntriesList(w http.ResponseWriter) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "entries-changed")
	data := struct{ Rows []Row }{Rows: buildRows(entries)}
	if err := tmpl.ExecuteTemplate(w, "entries-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	weightKg, err := strconv.ParseFloat(r.FormValue("weight_kg"), 64)
	if err != nil {
		http.Error(w, "invalid weight_kg", http.StatusBadRequest)
		return
	}
	recordedAt, err := parseRecordedAt(r)
	if err != nil {
		http.Error(w, "invalid recorded_at", http.StatusBadRequest)
		return
	}
	if _, err := db.CreateEntry(s.db, recordedAt, weightKg, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderEntriesList(w)
}

func (s *server) handleEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	entry, err := db.GetEntry(s.db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rows := buildRows([]db.Entry{entry})
	if err := tmpl.ExecuteTemplate(w, "row-edit", rows[0]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleCancelEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range buildRows(entries) {
		if row.ID == id {
			if err := tmpl.ExecuteTemplate(w, "row", row); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	weightKg, err := strconv.ParseFloat(r.FormValue("weight_kg"), 64)
	if err != nil {
		http.Error(w, "invalid weight_kg", http.StatusBadRequest)
		return
	}
	recordedAt, err := parseRecordedAt(r)
	if err != nil {
		http.Error(w, "invalid recorded_at", http.StatusBadRequest)
		return
	}
	if err := db.UpdateEntry(s.db, id, recordedAt, weightKg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderEntriesList(w)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteEntry(s.db, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderEntriesList(w)
}
