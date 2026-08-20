package main

import (
	"net/http"
	"time"

	"weight-tracker/internal/db"
)

// OvernightPair is one evening→morning reading pair — the same adjacency
// chronologicalWithDeltas establishes for the overnight delta, but capturing
// both sides' data for direct display rather than just the scalar delta.
type OvernightPair struct {
	MorningID         int64
	MorningRecordedAt time.Time // kept for windowing; not rendered directly (see MorningLabel)
	EveningLabel      string
	EveningWeightStr  string
	MorningLabel      string
	MorningWeightStr  string
	DeltaG            int64
	DeltaStr          string
	IsLoss            bool
}

// buildOvernightPairs walks chrono (already sorted ascending by RecordedAt,
// as chronologicalWithDeltas returns it) and, for each morning entry with a
// computed overnight delta, pairs it with the evening entry immediately
// preceding it — the same lastEvening chronologicalWithDeltas itself tracks,
// just retaining its data instead of only the resulting scalar. Returned
// newest-first, matching the History list's convention.
func buildOvernightPairs(chrono []db.Entry, overnightByID map[int64]int64) []OvernightPair {
	var pairs []OvernightPair
	var lastEvening *db.Entry
	for i := range chrono {
		e := &chrono[i]
		switch entryPeriod(*e) {
		case "morning":
			if delta, ok := overnightByID[e.ID]; ok && lastEvening != nil {
				pairs = append(pairs, OvernightPair{
					MorningID:         e.ID,
					MorningRecordedAt: e.RecordedAt,
					EveningLabel:      lastEvening.RecordedAt.Format("Jan 2, 2006 15:04"),
					EveningWeightStr:  formatKg(lastEvening.WeightG),
					MorningLabel:      e.RecordedAt.Format("Jan 2, 2006 15:04"),
					MorningWeightStr:  formatKg(e.WeightG),
					DeltaG:            delta,
					DeltaStr:          formatKgDelta(delta),
					IsLoss:            delta < 0,
				})
			}
		case "evening":
			lastEvening = e
		}
	}
	for i, j := 0, len(pairs)-1; i < j; i, j = i+1, j-1 {
		pairs[i], pairs[j] = pairs[j], pairs[i]
	}
	return pairs
}

// OvernightSummary aggregates the overnight deltas within a range. Grams are
// kept alongside formatted strings: the formatted values are for direct
// display, the grams for the client-side calculator's arithmetic (embedded as
// data attributes in templates/overnight.html).
type OvernightSummary struct {
	Empty string // set if no qualifying pairs in range
	Count int
	// AvgDeltaG is the "Typical" calculator basis.
	AvgDeltaG int64
	AvgStr    string
	AvgIsLoss bool
	// LastDeltaG is the most recent pair's delta — how last night compared.
	LastDeltaG int64
	LastStr    string
	LastIsLoss bool
	// BestCaseG is max(deltas): the smallest loss (or a gain) ever recorded —
	// the "Safe" calculator basis, since it's the worst case for making weight.
	BestCaseG      int64
	BestCaseStr    string
	BestCaseIsLoss bool
	// WorstCaseG is min(deltas): the largest loss ever recorded.
	WorstCaseG      int64
	WorstCaseStr    string
	WorstCaseIsLoss bool
}

func buildOvernightSummary(pairs []OvernightPair) OvernightSummary {
	if len(pairs) == 0 {
		return OvernightSummary{Empty: "Not enough data yet — log an evening weigh-in followed by the next morning's to see overnight stats."}
	}
	var sum int64
	best, worst := pairs[0].DeltaG, pairs[0].DeltaG
	for _, p := range pairs {
		sum += p.DeltaG
		best = max(best, p.DeltaG)
		worst = min(worst, p.DeltaG)
	}
	avg := sum / int64(len(pairs))
	last := pairs[0].DeltaG // pairs is newest-first
	return OvernightSummary{
		Count:           len(pairs),
		AvgDeltaG:       avg,
		AvgStr:          formatKgDelta(avg),
		AvgIsLoss:       avg < 0,
		LastDeltaG:      last,
		LastStr:         formatKgDelta(last),
		LastIsLoss:      last < 0,
		BestCaseG:       best,
		BestCaseStr:     formatKgDelta(best),
		BestCaseIsLoss:  best < 0,
		WorstCaseG:      worst,
		WorstCaseStr:    formatKgDelta(worst),
		WorstCaseIsLoss: worst < 0,
	}
}

// windowedOvernightPairs builds every overnight pair from the full entry set
// and only then filters down to window — chronologicalWithDeltas must run
// over the *full* entry set before any window filtering, the same rule
// filterRows depends on (entries.go): windowing the raw entries first would
// starve the adjacency check at the window's edges, e.g. dropping an evening
// entry that falls just outside the range but that the first visible
// morning entry's delta was computed against.
func windowedOvernightPairs(entries []db.Entry, window rangeWindow) []OvernightPair {
	chrono, overnightByID, _ := chronologicalWithDeltas(entries)
	allPairs := buildOvernightPairs(chrono, overnightByID)

	var pairs []OvernightPair
	for _, p := range allPairs {
		if window.contains(p.MorningRecordedAt) {
			pairs = append(pairs, p)
		}
	}
	return pairs
}

// handleOvernightTab renders the Overnight tab's stats/calculator/pairs-table
// fragment, filtered by the same range/from/until triple the chart and
// History filter submit via the shared time-range-picker.
func (s *server) handleOvernightTab(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "30"
	}
	window := resolveRangeWindow(rangeParam, r.URL.Query().Get("from"), r.URL.Query().Get("until"), time.Now())
	pairs := windowedOvernightPairs(entries, window)

	// Field names (Overnight/Pairs) match handleIndex's full data struct, so
	// the "overnight-content" template works from either — the same pattern
	// entries-list already uses (Rows) across handleIndex and
	// renderEntriesList's narrower struct.
	data := struct {
		Overnight OvernightSummary
		Pairs     []OvernightPair
	}{
		Overnight: buildOvernightSummary(pairs),
		Pairs:     pairs,
	}
	if err := tmpl.ExecuteTemplate(w, "overnight-content", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
