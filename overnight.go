package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
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

// overnightWindowSpans are the fixed trailing windows compared side by side
// in the "Range by timescale" chart — unlike the rest of the Overnight tab,
// this view isn't affected by the shared time-range-picker: seeing 7d/30d/90d
// side by side is the point, so it always computes all three regardless of
// whatever range the filter above is set to.
var overnightWindowSpans = []struct {
	Label string
	Days  int
}{
	{"7d", 7},
	{"30d", 30},
	{"90d", 90},
}

// OvernightWindowPoint is one window's box-plot-style entry in the "Range by
// timescale" chart: the box is the mean overnight delta ± 1 sample-standard-
// deviation, and the whiskers extend to the actual smallest/largest delta
// seen in that window — a hybrid of a real box plot's shape with this app's
// mean/stddev statistical basis rather than quartiles. HasRange is false
// below two pairs, since a standard deviation needs at least two samples to
// mean anything — the box then collapses to a single point at the mean
// rather than showing a fabricated zero-width band (Min/Max still equal
// Mean in that case, for the same reason).
type OvernightWindowPoint struct {
	Label     string  `json:"label"`
	Count     int     `json:"count"`
	HasRange  bool    `json:"hasRange"`
	MeanKg    float64 `json:"meanKg"`
	MeanLabel string  `json:"meanLabel"`
	LowKg     float64 `json:"lowKg"`
	HighKg    float64 `json:"highKg"`
	MinKg     float64 `json:"minKg"`
	MaxKg     float64 `json:"maxKg"`
}

type OvernightWindowChart struct {
	HasData bool                   `json:"hasData"`
	Empty   string                 `json:"empty,omitempty"`
	Points  []OvernightWindowPoint `json:"points,omitempty"`
	// HasGoal/GoalKg carry the currently-active goal weight (see
	// currentGoal in goals.go) so the client-side "tonight's weight"
	// calculator can judge whether a projected morning range clears it,
	// without a second round trip.
	HasGoal bool    `json:"hasGoal"`
	GoalKg  float64 `json:"goalKg,omitempty"`
}

// sampleStdDevG computes the sample standard deviation (n-1 denominator, the
// correct form for a sample rather than a full population) of deltas around
// meanG. ok is false below two values, matching HasRange above.
func sampleStdDevG(deltas []int64, meanG float64) (stddev float64, ok bool) {
	if len(deltas) < 2 {
		return 0, false
	}
	var sumSq float64
	for _, d := range deltas {
		diff := float64(d) - meanG
		sumSq += diff * diff
	}
	return math.Sqrt(sumSq / float64(len(deltas)-1)), true
}

// buildOvernightWindowChart computes the fixed-window comparison over the
// full entry set — windowedOvernightPairs already applies the same
// full-set-then-filter rule this file's other functions depend on.
func buildOvernightWindowChart(entries []db.Entry, goals []db.Goal, now time.Time) OvernightWindowChart {
	var points []OvernightWindowPoint
	anyData := false
	for _, span := range overnightWindowSpans {
		window := resolveRangeWindow(strconv.Itoa(span.Days), "", "", now)
		pairs := windowedOvernightPairs(entries, window)

		point := OvernightWindowPoint{Label: span.Label, Count: len(pairs)}
		if len(pairs) > 0 {
			anyData = true
			deltas := make([]int64, len(pairs))
			var sum int64
			minG, maxG := pairs[0].DeltaG, pairs[0].DeltaG
			for i, p := range pairs {
				deltas[i] = p.DeltaG
				sum += p.DeltaG
				minG = min(minG, p.DeltaG)
				maxG = max(maxG, p.DeltaG)
			}
			meanG := sum / int64(len(pairs))
			point.MeanKg = db.GramsToKg(meanG)
			point.MeanLabel = formatKgDelta(meanG)
			point.MinKg = db.GramsToKg(minG)
			point.MaxKg = db.GramsToKg(maxG)
			if stddev, ok := sampleStdDevG(deltas, float64(meanG)); ok {
				point.HasRange = true
				point.LowKg = db.GramsToKg(meanG - int64(math.Round(stddev)))
				point.HighKg = db.GramsToKg(meanG + int64(math.Round(stddev)))
			} else {
				point.LowKg, point.HighKg = point.MeanKg, point.MeanKg
			}
		}
		points = append(points, point)
	}

	if !anyData {
		return OvernightWindowChart{Empty: "Not enough data yet — log some overnight pairs to see how the range compares across timescales."}
	}

	chartData := OvernightWindowChart{HasData: true, Points: points}
	if goal, ok := currentGoal(goals, now); ok {
		chartData.HasGoal = true
		chartData.GoalKg = db.GramsToKg(goal.WeightG)
	}
	return chartData
}

// handleOvernightWindows returns the "Range by timescale" chart data as
// JSON, mirroring handleChart's split: no server-side pixel math, the
// client-side Chart.js instance handles that.
func (s *server) handleOvernightWindows(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	goals, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(buildOvernightWindowChart(entries, goals, time.Now())); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
