package overnight

import (
	"math"
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/testsupport"
	"weight-tracker/internal/timerange"
	"weight-tracker/internal/weight"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func entry(id int64, recordedAt time.Time, weightKg float64, override string) db.Entry {
	return testsupport.Entry(id, recordedAt, weightKg, override)
}

func goal(id int64, weightKg float64, effectiveFrom string, t *testing.T) db.Goal {
	return testsupport.Goal(id, weightKg, effectiveFrom, t)
}

func TestBuildPairs(t *testing.T) {
	t.Run("a genuine overnight adjacency produces one pair", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-14 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-15 07:00"), 82.5, ""),
		}
		chrono, overnightByID, _ := weight.ChronologicalWithDeltas(entries)
		pairs := BuildPairs(chrono, overnightByID)
		if len(pairs) != 1 {
			t.Fatalf("got %d pairs, want 1", len(pairs))
		}
		p := pairs[0]
		if p.MorningID != 2 {
			t.Errorf("MorningID = %d, want 2", p.MorningID)
		}
		if p.EveningWeightStr != "84.0" || p.MorningWeightStr != "82.5" {
			t.Errorf("weights = %q/%q, want 84.0/82.5", p.EveningWeightStr, p.MorningWeightStr)
		}
		if p.DeltaStr != "-1.5 kg" || !p.IsLoss {
			t.Errorf("delta = %q (IsLoss=%v), want -1.5 kg, loss", p.DeltaStr, p.IsLoss)
		}
	})

	t.Run("a gap in logging produces no pair", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-10 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-14 07:00"), 82.5, ""), // several days later
		}
		chrono, overnightByID, _ := weight.ChronologicalWithDeltas(entries)
		if pairs := BuildPairs(chrono, overnightByID); len(pairs) != 0 {
			t.Errorf("got %d pairs, want 0 across a multi-day gap", len(pairs))
		}
	})

	t.Run("a lone morning entry with no preceding evening produces no pair", func(t *testing.T) {
		entries := []db.Entry{entry(1, at(t, "2026-08-15 07:00"), 82.5, "")}
		chrono, overnightByID, _ := weight.ChronologicalWithDeltas(entries)
		if pairs := BuildPairs(chrono, overnightByID); len(pairs) != 0 {
			t.Errorf("got %d pairs, want 0 with no evening entry at all", len(pairs))
		}
	})

	t.Run("multiple pairs come back newest-first", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-14 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-15 07:00"), 82.5, ""),
			entry(3, at(t, "2026-08-17 20:00"), 85.0, ""),
			entry(4, at(t, "2026-08-18 07:00"), 83.9, ""),
		}
		chrono, overnightByID, _ := weight.ChronologicalWithDeltas(entries)
		pairs := BuildPairs(chrono, overnightByID)
		if len(pairs) != 2 {
			t.Fatalf("got %d pairs, want 2", len(pairs))
		}
		if pairs[0].MorningID != 4 || pairs[1].MorningID != 2 {
			t.Errorf("pair order = [%d, %d], want newest-first [4, 2]", pairs[0].MorningID, pairs[1].MorningID)
		}
	})
}

func TestBuildSummary(t *testing.T) {
	t.Run("no pairs yields an empty-state message", func(t *testing.T) {
		got := BuildSummary(nil)
		if got.Empty == "" {
			t.Error("Empty = \"\", want an explanatory message")
		}
	})

	t.Run("aggregates average, last, best-case, and worst-case", func(t *testing.T) {
		// BuildPairs returns newest-first; construct the fixture the same
		// way rather than relying on internal ordering assumptions.
		pairs := []Pair{
			{MorningID: 3, DeltaG: -1100, IsLoss: true}, // most recent — smallest loss (best case)
			{MorningID: 2, DeltaG: -1500, IsLoss: true}, // largest loss (worst case)
		}
		got := BuildSummary(pairs)

		if got.Count != 2 {
			t.Errorf("Count = %d, want 2", got.Count)
		}
		if got.AvgDeltaG != -1300 || !got.AvgIsLoss {
			t.Errorf("Avg = %d (IsLoss=%v), want -1300, loss", got.AvgDeltaG, got.AvgIsLoss)
		}
		if got.LastDeltaG != -1100 || !got.LastIsLoss {
			t.Errorf("Last = %d, want -1100 (the first/newest element)", got.LastDeltaG)
		}
		if got.BestCaseG != -1100 || !got.BestCaseIsLoss {
			t.Errorf("BestCase = %d, want -1100 (the smallest loss)", got.BestCaseG)
		}
		if got.WorstCaseG != -1500 || !got.WorstCaseIsLoss {
			t.Errorf("WorstCase = %d, want -1500 (the largest loss)", got.WorstCaseG)
		}
	})

	t.Run("a gain overnight is not a loss", func(t *testing.T) {
		got := BuildSummary([]Pair{{MorningID: 1, DeltaG: 200}})
		if got.AvgIsLoss || got.BestCaseIsLoss || got.WorstCaseIsLoss || got.LastIsLoss {
			t.Error("a positive delta was classified as a loss")
		}
	})
}

func TestWindowedPairs(t *testing.T) {
	entries := []db.Entry{
		entry(1, at(t, "2026-08-14 20:00"), 84.0, ""), // evening, just before the window below
		entry(2, at(t, "2026-08-15 07:00"), 82.5, ""), // morning, inside the window
	}

	t.Run("the morning entry's delta survives even when its evening partner is outside the window", func(t *testing.T) {
		// Mirrors the History-filter correctness rule this session already
		// hit once: windowing entries before computing deltas would starve
		// the adjacency check at the window's edge.
		window := timerange.Window{From: at(t, "2026-08-15 00:00"), HasFrom: true}
		pairs := WindowedPairs(entries, window)
		if len(pairs) != 1 {
			t.Fatalf("got %d pairs, want 1 (the morning entry's delta, computed against the off-window evening entry)", len(pairs))
		}
		if pairs[0].DeltaStr != "-1.5 kg" {
			t.Errorf("delta = %q, want -1.5 kg", pairs[0].DeltaStr)
		}
	})

	t.Run("a window excluding the morning entry too yields nothing", func(t *testing.T) {
		window := timerange.Window{From: at(t, "2026-08-16 00:00"), HasFrom: true}
		if pairs := WindowedPairs(entries, window); len(pairs) != 0 {
			t.Errorf("got %d pairs, want 0", len(pairs))
		}
	})

	t.Run("an unbounded window is a pass-through", func(t *testing.T) {
		if pairs := WindowedPairs(entries, timerange.Window{}); len(pairs) != 1 {
			t.Errorf("got %d pairs, want 1", len(pairs))
		}
	})
}

func TestSampleStdDev(t *testing.T) {
	t.Run("fewer than two values has no standard deviation", func(t *testing.T) {
		if _, ok := sampleStdDev(nil, 0); ok {
			t.Error("got ok=true for zero values")
		}
		if _, ok := sampleStdDev([]int64{-1500}, -1500); ok {
			t.Error("got ok=true for a single value")
		}
	})

	t.Run("matches a hand-computed sample standard deviation", func(t *testing.T) {
		// Deltas -1000, -1500, -1300g; mean -1266.67g. Squared deviations
		// 71111.1, 54444.4, 1111.1 sum to 126666.6 g^2; divided by n-1=2
		// that's a variance of 63333.3 g^2, so stddev is ~251.66g —
		// computed by hand here rather than trusting the implementation to
		// check itself.
		deltas := []int64{-1000, -1500, -1300}
		mean := -1266.6666666666667
		got, ok := sampleStdDev(deltas, mean)
		if !ok {
			t.Fatal("got ok=false, want true for 3 values")
		}
		if math.Abs(got-251.66) > 0.5 {
			t.Errorf("stddev = %.2f, want ~251.66", got)
		}
	})
}

func TestBuildWindowChart(t *testing.T) {
	t.Run("no entries at all yields the empty state", func(t *testing.T) {
		got := BuildWindowChart(nil, nil, at(t, "2026-08-20 12:00"))
		if got.HasData {
			t.Error("HasData = true, want false with no entries")
		}
		if got.Empty == "" {
			t.Error("Empty = \"\", want an explanatory message")
		}
	})

	t.Run("a single pair gives a mean but no range, and min/max collapse to it", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-19 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-20 07:00"), 82.5, ""),
		}
		got := BuildWindowChart(entries, nil, at(t, "2026-08-20 12:00"))
		if !got.HasData {
			t.Fatal("HasData = false, want true")
		}
		p := got.Points[0] // "7d"
		if p.Label != "7d" || p.Count != 1 {
			t.Fatalf("Points[0] = %+v, want label 7d, count 1", p)
		}
		if p.HasRange {
			t.Error("HasRange = true, want false with only one pair")
		}
		if p.LowKg != p.MeanKg || p.HighKg != p.MeanKg {
			t.Errorf("Low/High = %v/%v, want both equal to Mean %v", p.LowKg, p.HighKg, p.MeanKg)
		}
		if p.MinKg != p.MeanKg || p.MaxKg != p.MeanKg {
			t.Errorf("Min/Max = %v/%v, want both equal to Mean %v", p.MinKg, p.MaxKg, p.MeanKg)
		}
	})

	t.Run("wider windows accumulate more pairs than narrower ones", func(t *testing.T) {
		entries := []db.Entry{
			// Within the last 7 days.
			entry(1, at(t, "2026-08-18 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-19 07:00"), 82.5, ""),
			// Only within the last 30 days.
			entry(3, at(t, "2026-07-25 20:00"), 84.0, ""),
			entry(4, at(t, "2026-07-26 07:00"), 82.0, ""),
			// Only within the last 90 days.
			entry(5, at(t, "2026-06-01 20:00"), 84.0, ""),
			entry(6, at(t, "2026-06-02 07:00"), 81.0, ""),
		}
		now := at(t, "2026-08-20 12:00")
		got := BuildWindowChart(entries, nil, now)
		counts := map[string]int{}
		for _, p := range got.Points {
			counts[p.Label] = p.Count
		}
		if counts["7d"] != 1 || counts["30d"] != 2 || counts["90d"] != 3 {
			t.Errorf("counts = %+v, want 7d:1, 30d:2, 90d:3", counts)
		}
	})

	t.Run("the 90d window's whiskers span its actual min/max delta", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-18 20:00"), 84.0, ""), // -1.5kg
			entry(2, at(t, "2026-08-19 07:00"), 82.5, ""),
			entry(3, at(t, "2026-07-25 20:00"), 84.0, ""), // -3.0kg (largest loss)
			entry(4, at(t, "2026-07-26 07:00"), 81.0, ""),
			entry(5, at(t, "2026-06-01 20:00"), 84.0, ""), // +0.5kg (a gain)
			entry(6, at(t, "2026-06-02 07:00"), 84.5, ""),
		}
		got := BuildWindowChart(entries, nil, at(t, "2026-08-20 12:00"))
		var ninety WindowPoint
		for _, p := range got.Points {
			if p.Label == "90d" {
				ninety = p
			}
		}
		if ninety.MinKg != -3.0 || ninety.MaxKg != 0.5 {
			t.Errorf("90d Min/Max = %v/%v, want -3.0/0.5", ninety.MinKg, ninety.MaxKg)
		}
	})

	t.Run("no goal set means HasGoal is false", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-19 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-20 07:00"), 82.5, ""),
		}
		got := BuildWindowChart(entries, nil, at(t, "2026-08-20 12:00"))
		if got.HasGoal {
			t.Error("HasGoal = true, want false with no goals set")
		}
	})

	t.Run("an active goal is surfaced alongside the window stats", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-19 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-20 07:00"), 82.5, ""),
		}
		goalList := []db.Goal{goal(1, 80, "2026-01-01", t)}
		got := BuildWindowChart(entries, goalList, at(t, "2026-08-20 12:00"))
		if !got.HasGoal {
			t.Fatal("HasGoal = false, want true with an active goal")
		}
		if got.GoalKg != 80 {
			t.Errorf("GoalKg = %v, want 80", got.GoalKg)
		}
	})
}

// TestBuildWindowChartDropsWindowsMatchingTheOneBefore covers the rule that
// a timescale is only offered when it actually covers more nights than the
// previous one. A 90-day window over ten days of history holds exactly the
// nights the 30-day window does, so it computes an identical mean, range
// and whiskers — a second column inviting the reader to find a difference
// that cannot exist.
func TestBuildWindowChartDropsWindowsMatchingTheOneBefore(t *testing.T) {
	now := at(t, "2026-08-22 12:00")

	// nightsOfHistory builds an evening/morning pair for each of the last n
	// days, which is what WindowedPairs counts.
	nightsOfHistory := func(n int) []db.Entry {
		var entries []db.Entry
		var id int64
		for d := n; d >= 1; d-- {
			evening := now.AddDate(0, 0, -d)
			morning := evening.AddDate(0, 0, 1)
			id++
			entries = append(entries, testsupport.Entry(id,
				time.Date(evening.Year(), evening.Month(), evening.Day(), 21, 0, 0, 0, evening.Location()), 84.0, ""))
			id++
			entries = append(entries, testsupport.Entry(id,
				time.Date(morning.Year(), morning.Month(), morning.Day(), 7, 0, 0, 0, morning.Location()), 83.0, ""))
		}
		return entries
	}

	labelsFor := func(days int) []string {
		var labels []string
		for _, p := range BuildWindowChart(nightsOfHistory(days), nil, now).Points {
			labels = append(labels, p.Label)
		}
		return labels
	}

	tests := []struct {
		days int
		want []string
	}{
		// Everything logged fits inside a week, so every longer window is
		// the same week.
		{3, []string{"7d"}},
		// The example that prompted this: 30d sees more than 7d, but 90d,
		// 1y and all-time see exactly what 30d sees.
		{10, []string{"7d", "30d"}},
		{45, []string{"7d", "30d", "90d"}},
		{120, []string{"7d", "30d", "90d", "1y"}},
		{400, []string{"7d", "30d", "90d", "1y", "All"}},
	}
	for _, tc := range tests {
		got := labelsFor(tc.days)
		if len(got) != len(tc.want) {
			t.Errorf("%d nights: got %v, want %v", tc.days, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%d nights: got %v, want %v", tc.days, got, tc.want)
				break
			}
		}
	}
}

func TestBuildWindowChartNamesEveryWindow(t *testing.T) {
	now := at(t, "2026-08-22 12:00")
	entries := []db.Entry{
		entry(1, at(t, "2026-08-20 21:00"), 84.0, ""),
		entry(2, at(t, "2026-08-21 07:00"), 83.0, ""),
	}
	// The toggle beside the chart is built from these, so a window without a
	// spelled-out name would render as a bare "7d" checkbox.
	for _, p := range BuildWindowChart(entries, nil, now).Points {
		if p.Name == "" {
			t.Errorf("window %q has no display name", p.Label)
		}
	}
}
