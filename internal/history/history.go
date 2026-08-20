// Package history builds the display-ready Row form of the weigh-in list —
// the History tab's table — including each row's overnight/daily delta
// chips and the period/date-range filter applied to it.
package history

import (
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/timerange"
	"weight-tracker/internal/weight"
)

// Row is the display-ready form of a db.Entry. Period is weight.EntryPeriod's
// result: the user's PeriodOverride if they set one, else auto-detected
// from RecordedAt at render time via db.DetectPeriod.
type Row struct {
	ID              int64
	RecordedAt      time.Time // kept for FilterRows; not rendered directly (see RecordedAtLabel)
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

// BuildRows converts entries into display-ready rows, including each row's
// overnight/daily delta chip.
func BuildRows(entries []db.Entry) []Row {
	_, overnightByID, dailyByID := weight.ChronologicalWithDeltas(entries)

	rows := make([]Row, len(entries))
	for i, e := range entries {
		period := weight.EntryPeriod(e)
		r := Row{
			ID:              e.ID,
			RecordedAt:      e.RecordedAt,
			RecordedAtLabel: e.RecordedAt.Format("Jan 2, 2006 15:04"),
			RecordedAtDate:  e.RecordedAt.Format("2006-01-02"),
			RecordedAtTime:  e.RecordedAt.Format("15:04"),
			Period:          period,
			PeriodOverride:  e.PeriodOverride,
			WeightKgRaw:     weight.FormatKgInput(e.WeightG),
			WeightKgStr:     weight.FormatKg(e.WeightG),
		}
		if period == "morning" {
			r.PeriodLabel = "Morning"
		} else {
			r.PeriodLabel = "Evening"
		}
		if delta, ok := overnightByID[e.ID]; ok {
			r.OvernightLoss = delta < 0
			r.OvernightDelta = weight.FormatKgDelta(delta)
		}
		if delta, ok := dailyByID[e.ID]; ok {
			r.DailyGain = delta > 0
			r.DailyDelta = weight.FormatKgDelta(delta)
		}
		rows[i] = r
	}
	return rows
}

// FilterRows keeps rows whose period matches periodParam ("" or "all" for
// no filter) and whose RecordedAt falls within window.
//
// This runs on the output of BuildRows rather than filtering the entries
// beforehand, since the overnight/daily deltas need the *full* chronological
// context to compute correctly — filtering to "morning only" before
// weight.ChronologicalWithDeltas ran would starve it of the evening entries
// an overnight delta is computed against, silently dropping every chip.
func FilterRows(rows []Row, periodParam string, window timerange.Window) []Row {
	var out []Row
	for _, row := range rows {
		if periodParam != "" && periodParam != "all" && row.Period != periodParam {
			continue
		}
		if !window.Contains(row.RecordedAt) {
			continue
		}
		out = append(out, row)
	}
	return out
}
