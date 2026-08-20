package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"weight-tracker/internal/db"
)

// Row is the display-ready form of a db.Entry. Period is entryPeriod's
// result: the user's PeriodOverride if they set one, else auto-detected
// from RecordedAt at render time via db.DetectPeriod.
type Row struct {
	ID              int64
	RecordedAt      time.Time // kept for filterRows; not rendered directly (see RecordedAtLabel)
	RecordedAtLabel string
	RecordedAtDate  string // for the edit form's date input
	RecordedAtTime  string // for the edit form's time input
	Period          string
	PeriodLabel     string
	PeriodOverride  string // "", "morning", or "evening" — for the edit form's select
	WeightKgRaw     string
	WeightKgStr     string
	OvernightDelta  string // set on morning entries: vs. the prior evening
	OvernightLoss   bool
	DailyDelta      string // set on evening entries: vs. that same day's morning
	DailyGain       bool
}

// entryPeriod returns e's effective period: PeriodOverride if the user set
// one, otherwise auto-detected from RecordedAt via db.DetectPeriod.
func entryPeriod(e db.Entry) string {
	if e.PeriodOverride != "" {
		return e.PeriodOverride
	}
	return db.DetectPeriod(e.RecordedAt)
}

// validPeriodOverride reports whether s is a legal period_override value:
// "" (auto), "morning", or "evening".
func validPeriodOverride(s string) bool {
	return s == "" || s == "morning" || s == "evening"
}

// chronologicalWithDeltas orders entries by RecordedAt and, keyed by entry
// ID, computes two deltas:
//   - overnight: a morning entry's weight vs. the most recent evening entry
//     seen so far, but only if that evening entry was actually the
//     preceding logical day — otherwise there's a gap (e.g. no evening
//     logged in between) and the two readings aren't really "overnight"
//     from each other, so no delta is shown.
//   - daily: an evening entry's weight vs. the most recent morning entry
//     seen so far on that SAME logical day, i.e. same-day weight gained.
//
// "Logical day" (see logicalDate) rather than literal calendar date, since
// db.DetectPeriod's morning cutoff is 4am, not midnight: a 1:25am reading
// is labeled "evening" but shares its literal calendar date with the
// morning reading that follows it a few hours later — comparing literal
// dates would miss that pairing entirely.
//
// Both deltas are in grams, so they are exact differences of the stored
// values rather than the accumulated float error two kilogram subtractions
// used to produce.
func chronologicalWithDeltas(entries []db.Entry) (chrono []db.Entry, overnight, daily map[int64]int64) {
	chrono = make([]db.Entry, len(entries))
	copy(chrono, entries)
	sort.SliceStable(chrono, func(i, j int) bool {
		return chrono[i].RecordedAt.Before(chrono[j].RecordedAt)
	})

	overnight = make(map[int64]int64, len(entries))
	daily = make(map[int64]int64, len(entries))
	var lastMorning, lastEvening *db.Entry
	for i := range chrono {
		e := &chrono[i]
		switch entryPeriod(*e) {
		case "morning":
			if lastEvening != nil && isNextCalendarDay(lastEvening.RecordedAt, e.RecordedAt) {
				overnight[e.ID] = e.WeightG - lastEvening.WeightG
			}
			lastMorning = e
		case "evening":
			if lastMorning != nil && sameDate(lastMorning.RecordedAt, e.RecordedAt) {
				daily[e.ID] = e.WeightG - lastMorning.WeightG
			}
			lastEvening = e
		}
	}
	return chrono, overnight, daily
}

// logicalDate returns the calendar date t counts as belonging to for
// same-day/next-day comparisons, using the same 4am boundary as
// db.DetectPeriod's morning cutoff: a time before 4am is attributed to the
// previous calendar date, since it's still "last night" rather than the
// start of a new day.
func logicalDate(t time.Time) time.Time {
	if t.Hour() < 4 {
		t = t.AddDate(0, 0, -1)
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func sameDate(a, b time.Time) bool {
	return logicalDate(a).Equal(logicalDate(b))
}

// isNextCalendarDay reports whether b falls on the logical day immediately
// after a — used to require a genuine "yesterday evening to this morning"
// adjacency for the overnight delta, rather than comparing against
// whatever evening entry happens to be most recent regardless of gap size.
func isNextCalendarDay(a, b time.Time) bool {
	return logicalDate(a).AddDate(0, 0, 1).Equal(logicalDate(b))
}

// formatKg renders stored grams as the one-decimal kilogram string shown
// throughout the UI — the precision a bathroom scale actually reports.
func formatKg(grams int64) string {
	return fmt.Sprintf("%.1f", db.GramsToKg(grams))
}

// formatKgDelta renders a gram difference as a signed kilogram string.
func formatKgDelta(grams int64) string {
	return fmt.Sprintf("%+.1f kg", db.GramsToKg(grams))
}

// formatKgInput renders stored grams for a number input, keeping every
// digit that was stored (at most three decimals) so re-saving an unedited
// row cannot silently round it — a display-rounded 82.4 would otherwise
// overwrite an imported 82.437.
func formatKgInput(grams int64) string {
	return strconv.FormatFloat(db.GramsToKg(grams), 'f', -1, 64)
}

func buildRows(entries []db.Entry) []Row {
	_, overnightByID, dailyByID := chronologicalWithDeltas(entries)

	rows := make([]Row, len(entries))
	for i, e := range entries {
		period := entryPeriod(e)
		r := Row{
			ID:              e.ID,
			RecordedAt:      e.RecordedAt,
			RecordedAtLabel: e.RecordedAt.Format("Jan 2, 2006 15:04"),
			RecordedAtDate:  e.RecordedAt.Format("2006-01-02"),
			RecordedAtTime:  e.RecordedAt.Format("15:04"),
			Period:          period,
			PeriodOverride:  e.PeriodOverride,
			WeightKgRaw:     formatKgInput(e.WeightG),
			WeightKgStr:     formatKg(e.WeightG),
		}
		if period == "morning" {
			r.PeriodLabel = "Morning"
		} else {
			r.PeriodLabel = "Evening"
		}
		if delta, ok := overnightByID[e.ID]; ok {
			r.OvernightLoss = delta < 0
			r.OvernightDelta = formatKgDelta(delta)
		}
		if delta, ok := dailyByID[e.ID]; ok {
			r.DailyGain = delta > 0
			r.DailyDelta = formatKgDelta(delta)
		}
		rows[i] = r
	}
	return rows
}

// filterRows keeps rows whose period matches periodParam ("" or "all" for
// no filter) and whose RecordedAt falls within window.
//
// This runs on the output of buildRows rather than filtering the entries
// beforehand, since the overnight/daily deltas need the *full* chronological
// context to compute correctly — filtering to "morning only" before
// chronologicalWithDeltas ran would starve it of the evening entries an
// overnight delta is computed against, silently dropping every chip.
func filterRows(rows []Row, periodParam string, window rangeWindow) []Row {
	var out []Row
	for _, row := range rows {
		if periodParam != "" && periodParam != "all" && row.Period != periodParam {
			continue
		}
		if !window.contains(row.RecordedAt) {
			continue
		}
		out = append(out, row)
	}
	return out
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

// handleEntriesList renders the history list filtered by the entries-filter
// form — period, plus the exact same range/from/until triple the chart's
// own time-range-picker submits (it's the same shared component), so a
// preset like range=30 is resolved by resolveRangeWindow the same way for
// both. Registered separately from the CRUD handlers above so a
// create/update/delete's own response can keep rendering the unfiltered
// list — the filter form re-applies itself afterward via
// hx-trigger="... entries-changed from:body", so the visible list ends up
// filtered either way, just via one extra round-trip.
func (s *server) handleEntriesList(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	periodParam := r.URL.Query().Get("period")
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "all"
	}
	window := resolveRangeWindow(rangeParam, r.URL.Query().Get("from"), r.URL.Query().Get("until"), time.Now())
	data := struct{ Rows []Row }{Rows: filterRows(buildRows(entries), periodParam, window)}
	if err := tmpl.ExecuteTemplate(w, "entries-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	weightG, err := parseWeightG(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recordedAt, err := parseRecordedAt(r)
	if err != nil {
		http.Error(w, "invalid recorded_at", http.StatusBadRequest)
		return
	}
	periodOverride := r.FormValue("period_override")
	if !validPeriodOverride(periodOverride) {
		http.Error(w, "invalid period_override", http.StatusBadRequest)
		return
	}
	if _, err := db.CreateEntry(s.db, recordedAt, weightG, periodOverride, time.Now()); err != nil {
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
	weightG, err := parseWeightG(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recordedAt, err := parseRecordedAt(r)
	if err != nil {
		http.Error(w, "invalid recorded_at", http.StatusBadRequest)
		return
	}
	periodOverride := r.FormValue("period_override")
	if !validPeriodOverride(periodOverride) {
		http.Error(w, "invalid period_override", http.StatusBadRequest)
		return
	}
	if err := db.UpdateEntry(s.db, id, recordedAt, weightG, periodOverride); err != nil {
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
		writeDeleteError(w, err)
		return
	}
	s.renderEntriesList(w)
}
