package main

import (
	"math"
	"testing"

	"weight-tracker/internal/db"
)

func TestBuildOvernightPairs(t *testing.T) {
	t.Run("a genuine overnight adjacency produces one pair", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-14 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-15 07:00"), 82.5, ""),
		}
		chrono, overnightByID, _ := chronologicalWithDeltas(entries)
		pairs := buildOvernightPairs(chrono, overnightByID)
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
		chrono, overnightByID, _ := chronologicalWithDeltas(entries)
		if pairs := buildOvernightPairs(chrono, overnightByID); len(pairs) != 0 {
			t.Errorf("got %d pairs, want 0 across a multi-day gap", len(pairs))
		}
	})

	t.Run("a lone morning entry with no preceding evening produces no pair", func(t *testing.T) {
		entries := []db.Entry{entry(1, at(t, "2026-08-15 07:00"), 82.5, "")}
		chrono, overnightByID, _ := chronologicalWithDeltas(entries)
		if pairs := buildOvernightPairs(chrono, overnightByID); len(pairs) != 0 {
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
		chrono, overnightByID, _ := chronologicalWithDeltas(entries)
		pairs := buildOvernightPairs(chrono, overnightByID)
		if len(pairs) != 2 {
			t.Fatalf("got %d pairs, want 2", len(pairs))
		}
		if pairs[0].MorningID != 4 || pairs[1].MorningID != 2 {
			t.Errorf("pair order = [%d, %d], want newest-first [4, 2]", pairs[0].MorningID, pairs[1].MorningID)
		}
	})
}

func TestBuildOvernightSummary(t *testing.T) {
	t.Run("no pairs yields an empty-state message", func(t *testing.T) {
		got := buildOvernightSummary(nil)
		if got.Empty == "" {
			t.Error("Empty = \"\", want an explanatory message")
		}
	})

	t.Run("aggregates average, last, best-case, and worst-case", func(t *testing.T) {
		// buildOvernightPairs returns newest-first; construct the fixture the
		// same way rather than relying on internal ordering assumptions.
		pairs := []OvernightPair{
			{MorningID: 3, DeltaG: -1100, IsLoss: true}, // most recent — smallest loss (best case)
			{MorningID: 2, DeltaG: -1500, IsLoss: true}, // largest loss (worst case)
		}
		got := buildOvernightSummary(pairs)

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
		got := buildOvernightSummary([]OvernightPair{{MorningID: 1, DeltaG: 200}})
		if got.AvgIsLoss || got.BestCaseIsLoss || got.WorstCaseIsLoss || got.LastIsLoss {
			t.Error("a positive delta was classified as a loss")
		}
	})
}

func TestSampleStdDevG(t *testing.T) {
	t.Run("fewer than two values has no standard deviation", func(t *testing.T) {
		if _, ok := sampleStdDevG(nil, 0); ok {
			t.Error("got ok=true for zero values")
		}
		if _, ok := sampleStdDevG([]int64{-1500}, -1500); ok {
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
		got, ok := sampleStdDevG(deltas, mean)
		if !ok {
			t.Fatal("got ok=false, want true for 3 values")
		}
		if math.Abs(got-251.66) > 0.5 {
			t.Errorf("stddev = %.2f, want ~251.66", got)
		}
	})
}

func TestBuildOvernightWindowChart(t *testing.T) {
	t.Run("no entries at all yields the empty state", func(t *testing.T) {
		got := buildOvernightWindowChart(nil, at(t, "2026-08-20 12:00"))
		if got.HasData {
			t.Error("HasData = true, want false with no entries")
		}
		if got.Empty == "" {
			t.Error("Empty = \"\", want an explanatory message")
		}
	})

	t.Run("a single pair gives a mean but no range", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-19 20:00"), 84.0, ""),
			entry(2, at(t, "2026-08-20 07:00"), 82.5, ""),
		}
		got := buildOvernightWindowChart(entries, at(t, "2026-08-20 12:00"))
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
		got := buildOvernightWindowChart(entries, now)
		counts := map[string]int{}
		for _, p := range got.Points {
			counts[p.Label] = p.Count
		}
		if counts["7d"] != 1 || counts["30d"] != 2 || counts["90d"] != 3 {
			t.Errorf("counts = %+v, want 7d:1, 30d:2, 90d:3", counts)
		}
	})
}

func TestWindowedOvernightPairs(t *testing.T) {
	entries := []db.Entry{
		entry(1, at(t, "2026-08-14 20:00"), 84.0, ""), // evening, just before the window below
		entry(2, at(t, "2026-08-15 07:00"), 82.5, ""), // morning, inside the window
	}

	t.Run("the morning entry's delta survives even when its evening partner is outside the window", func(t *testing.T) {
		// Mirrors the History-filter correctness rule this session already
		// hit once: windowing entries before computing deltas would starve
		// the adjacency check at the window's edge.
		window := rangeWindow{from: at(t, "2026-08-15 00:00"), hasFrom: true}
		pairs := windowedOvernightPairs(entries, window)
		if len(pairs) != 1 {
			t.Fatalf("got %d pairs, want 1 (the morning entry's delta, computed against the off-window evening entry)", len(pairs))
		}
		if pairs[0].DeltaStr != "-1.5 kg" {
			t.Errorf("delta = %q, want -1.5 kg", pairs[0].DeltaStr)
		}
	})

	t.Run("a window excluding the morning entry too yields nothing", func(t *testing.T) {
		window := rangeWindow{from: at(t, "2026-08-16 00:00"), hasFrom: true}
		if pairs := windowedOvernightPairs(entries, window); len(pairs) != 0 {
			t.Errorf("got %d pairs, want 0", len(pairs))
		}
	})

	t.Run("an unbounded window is a pass-through", func(t *testing.T) {
		if pairs := windowedOvernightPairs(entries, rangeWindow{}); len(pairs) != 1 {
			t.Errorf("got %d pairs, want 1", len(pairs))
		}
	})
}
