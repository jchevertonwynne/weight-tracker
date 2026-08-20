// Package weight holds the morning/evening period logic, the
// overnight/daily delta engine built on it, and the kilogram formatting
// helpers shared across every other domain package.
package weight

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"weight-tracker/internal/db"
)

// EntryPeriod returns e's effective period: PeriodOverride if the user set
// one, otherwise auto-detected from RecordedAt via db.DetectPeriod.
func EntryPeriod(e db.Entry) string {
	if e.PeriodOverride != "" {
		return e.PeriodOverride
	}
	return db.DetectPeriod(e.RecordedAt)
}

// ValidPeriodOverride reports whether s is a legal period_override value:
// "" (auto), "morning", or "evening".
func ValidPeriodOverride(s string) bool {
	return s == "" || s == "morning" || s == "evening"
}

// ChronologicalWithDeltas orders entries by RecordedAt and, keyed by entry
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
func ChronologicalWithDeltas(entries []db.Entry) (chrono []db.Entry, overnight, daily map[int64]int64) {
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
		switch EntryPeriod(*e) {
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

// FormatKg renders stored grams as the one-decimal kilogram string shown
// throughout the UI — the precision a bathroom scale actually reports.
func FormatKg(grams int64) string {
	return fmt.Sprintf("%.1f", db.GramsToKg(grams))
}

// FormatKgDelta renders a gram difference as a signed kilogram string.
func FormatKgDelta(grams int64) string {
	return fmt.Sprintf("%+.1f kg", db.GramsToKg(grams))
}

// FormatKgInput renders stored grams for a number input, keeping every
// digit that was stored (at most three decimals) so re-saving an unedited
// row cannot silently round it — a display-rounded 82.4 would otherwise
// overwrite an imported 82.437.
func FormatKgInput(grams int64) string {
	return strconv.FormatFloat(db.GramsToKg(grams), 'f', -1, 64)
}
