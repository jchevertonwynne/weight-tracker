// Package overnight computes the evening-to-morning weight change: the
// per-pair deltas, a filtered-range summary, and a fixed 7d/30d/90d
// box-plot-style comparison used by the Overnight tab.
package overnight

import (
	"math"
	"strconv"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/goals"
	"weight-tracker/internal/timerange"
	"weight-tracker/internal/weight"
)

// Pair is one evening→morning reading pair — the same adjacency
// weight.ChronologicalWithDeltas establishes for the overnight delta, but
// capturing both sides' data for direct display rather than just the
// scalar delta.
type Pair struct {
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

// BuildPairs walks chrono (already sorted ascending by RecordedAt, as
// weight.ChronologicalWithDeltas returns it) and, for each morning entry
// with a computed overnight delta, pairs it with the evening entry
// immediately preceding it — the same "last evening" tracking
// ChronologicalWithDeltas itself does, just retaining its data instead of
// only the resulting scalar. Returned newest-first, matching the History
// list's convention.
func BuildPairs(chrono []db.Entry, overnightByID map[int64]int64) []Pair {
	var pairs []Pair
	var lastEvening *db.Entry
	for i := range chrono {
		e := &chrono[i]
		switch weight.EntryPeriod(*e) {
		case "morning":
			if delta, ok := overnightByID[e.ID]; ok && lastEvening != nil {
				pairs = append(pairs, Pair{
					MorningID:         e.ID,
					MorningRecordedAt: e.RecordedAt,
					EveningLabel:      lastEvening.RecordedAt.Format("Jan 2, 2006 15:04"),
					EveningWeightStr:  weight.FormatKg(lastEvening.WeightG),
					MorningLabel:      e.RecordedAt.Format("Jan 2, 2006 15:04"),
					MorningWeightStr:  weight.FormatKg(e.WeightG),
					DeltaG:            delta,
					DeltaStr:          weight.FormatKgDelta(delta),
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

// Summary aggregates the overnight deltas within a range. Grams are kept
// alongside formatted strings: the formatted values are for direct display,
// the grams for the client-side calculator's arithmetic (embedded as data
// attributes in templates/overnight.html).
type Summary struct {
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

func BuildSummary(pairs []Pair) Summary {
	if len(pairs) == 0 {
		return Summary{Empty: "Not enough data yet — log an evening weigh-in followed by the next morning's to see overnight stats."}
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
	return Summary{
		Count:           len(pairs),
		AvgDeltaG:       avg,
		AvgStr:          weight.FormatKgDelta(avg),
		AvgIsLoss:       avg < 0,
		LastDeltaG:      last,
		LastStr:         weight.FormatKgDelta(last),
		LastIsLoss:      last < 0,
		BestCaseG:       best,
		BestCaseStr:     weight.FormatKgDelta(best),
		BestCaseIsLoss:  best < 0,
		WorstCaseG:      worst,
		WorstCaseStr:    weight.FormatKgDelta(worst),
		WorstCaseIsLoss: worst < 0,
	}
}

// WindowedPairs builds every overnight pair from the full entry set and
// only then filters down to window — weight.ChronologicalWithDeltas must
// run over the *full* entry set before any window filtering, the same rule
// the history list depends on: windowing the raw entries first would
// starve the adjacency check at the window's edges, e.g. dropping an
// evening entry that falls just outside the range but that the first
// visible morning entry's delta was computed against.
func WindowedPairs(entries []db.Entry, window timerange.Window) []Pair {
	chrono, overnightByID, _ := weight.ChronologicalWithDeltas(entries)
	allPairs := BuildPairs(chrono, overnightByID)

	var pairs []Pair
	for _, p := range allPairs {
		if window.Contains(p.MorningRecordedAt) {
			pairs = append(pairs, p)
		}
	}
	return pairs
}

// windowSpans are the fixed trailing windows compared side by side in the
// "Range by timescale" chart — unlike the rest of the Overnight tab, this
// view isn't affected by the shared time-range-picker: seeing 7d/30d/90d
// side by side is the point, so it always computes all three regardless of
// whatever range the filter above is set to.
var windowSpans = []struct {
	Label string
	Days  int
}{
	{"7d", 7},
	{"30d", 30},
	{"90d", 90},
}

// WindowPoint is one window's box-plot-style entry in the "Range by
// timescale" chart: the box is the mean overnight delta ± 1 sample-standard-
// deviation, and the whiskers extend to the actual smallest/largest delta
// seen in that window — a hybrid of a real box plot's shape with this app's
// mean/stddev statistical basis rather than quartiles. HasRange is false
// below two pairs, since a standard deviation needs at least two samples to
// mean anything — the box then collapses to a single point at the mean
// rather than showing a fabricated zero-width band (Min/Max still equal
// Mean in that case, for the same reason).
type WindowPoint struct {
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

type WindowChart struct {
	HasData bool          `json:"hasData"`
	Empty   string        `json:"empty,omitempty"`
	Points  []WindowPoint `json:"points,omitempty"`
	// HasGoal/GoalKg carry the currently-active goal weight (see
	// goals.Current) so the client-side "tonight's weight" calculator can
	// judge whether a projected morning range clears it, without a second
	// round trip.
	HasGoal bool    `json:"hasGoal"`
	GoalKg  float64 `json:"goalKg,omitempty"`
}

// sampleStdDev computes the sample standard deviation (n-1 denominator, the
// correct form for a sample rather than a full population) of deltas around
// meanG. ok is false below two values, matching HasRange above.
func sampleStdDev(deltas []int64, meanG float64) (stddev float64, ok bool) {
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

// BuildWindowChart computes the fixed-window comparison over the full
// entry set — WindowedPairs already applies the same full-set-then-filter
// rule this package's other functions depend on.
func BuildWindowChart(entries []db.Entry, allGoals []db.Goal, now time.Time) WindowChart {
	var points []WindowPoint
	anyData := false
	for _, span := range windowSpans {
		window := timerange.Resolve(strconv.Itoa(span.Days), "", "", now)
		pairs := WindowedPairs(entries, window)

		point := WindowPoint{Label: span.Label, Count: len(pairs)}
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
			point.MeanLabel = weight.FormatKgDelta(meanG)
			point.MinKg = db.GramsToKg(minG)
			point.MaxKg = db.GramsToKg(maxG)
			if stddev, ok := sampleStdDev(deltas, float64(meanG)); ok {
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
		return WindowChart{Empty: "Not enough data yet — log some overnight pairs to see how the range compares across timescales."}
	}

	chartData := WindowChart{HasData: true, Points: points}
	if goal, ok := goals.Current(allGoals, now); ok {
		chartData.HasGoal = true
		chartData.GoalKg = db.GramsToKg(goal.WeightG)
	}
	return chartData
}
