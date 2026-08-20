package main

import (
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
