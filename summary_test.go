package main

import (
	"testing"

	"weight-tracker/internal/db"
)

func TestBuildWeeklySummary(t *testing.T) {
	now := at(t, "2026-08-16 12:00")

	t.Run("compares this week's morning average to last week's", func(t *testing.T) {
		entries := []db.Entry{
			// This week (within 7 days of now).
			entry(1, at(t, "2026-08-15 07:00"), 82.0, ""),
			entry(2, at(t, "2026-08-13 07:00"), 82.4, ""),
			// Last week (7-14 days back).
			entry(3, at(t, "2026-08-08 07:00"), 83.0, ""),
			entry(4, at(t, "2026-08-06 07:00"), 83.4, ""),
		}
		got := buildWeeklySummary(entries, now)
		if got.Empty != "" {
			t.Fatalf("Empty = %q, want a populated summary", got.Empty)
		}
		if !got.HasComparison {
			t.Fatal("HasComparison = false, want true")
		}
		if got.ThisWeekAvg != "82.2 kg" {
			t.Errorf("ThisWeekAvg = %q, want %q", got.ThisWeekAvg, "82.2 kg")
		}
		if got.LastWeekAvg != "83.2 kg" {
			t.Errorf("LastWeekAvg = %q, want %q", got.LastWeekAvg, "83.2 kg")
		}
		if got.Delta != "-1.0 kg" {
			t.Errorf("Delta = %q, want %q", got.Delta, "-1.0 kg")
		}
		if !got.DeltaIsLoss {
			t.Error("DeltaIsLoss = false, want true")
		}
	})

	t.Run("ignores evening entries", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 07:00"), 82.0, ""),
			entry(2, at(t, "2026-08-15 21:00"), 90.0, ""), // must not skew the average
		}
		got := buildWeeklySummary(entries, now)
		if got.ThisWeekAvg != "82.0 kg" {
			t.Errorf("ThisWeekAvg = %q, want the morning entry alone (82.0 kg)", got.ThisWeekAvg)
		}
	})

	t.Run("respects a period override when selecting entries", func(t *testing.T) {
		entries := []db.Entry{
			// Auto-detects as evening, overridden to morning, so it counts.
			entry(1, at(t, "2026-08-15 21:00"), 82.0, "morning"),
		}
		got := buildWeeklySummary(entries, now)
		if got.ThisWeekAvg != "82.0 kg" {
			t.Errorf("ThisWeekAvg = %q, want the overridden entry to count", got.ThisWeekAvg)
		}
	})

	t.Run("reports a gain with a plus sign", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 07:00"), 83.0, ""),
			entry(2, at(t, "2026-08-08 07:00"), 82.0, ""),
		}
		got := buildWeeklySummary(entries, now)
		if got.Delta != "+1.0 kg" {
			t.Errorf("Delta = %q, want %q", got.Delta, "+1.0 kg")
		}
		if got.DeltaIsLoss {
			t.Error("DeltaIsLoss = true for a gain")
		}
	})

	t.Run("no comparison when last week has no morning entries", func(t *testing.T) {
		entries := []db.Entry{entry(1, at(t, "2026-08-15 07:00"), 82.0, "")}
		got := buildWeeklySummary(entries, now)
		if got.ThisWeekAvg == "" {
			t.Error("ThisWeekAvg is blank, want this week's average")
		}
		if got.HasComparison {
			t.Error("HasComparison = true with no data last week")
		}
		if got.Delta != "" {
			t.Errorf("Delta = %q, want blank", got.Delta)
		}
	})

	t.Run("entries older than two weeks are excluded entirely", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 07:00"), 82.0, ""),
			entry(2, at(t, "2026-06-01 07:00"), 95.0, ""), // ancient, must not count
		}
		got := buildWeeklySummary(entries, now)
		if got.HasComparison {
			t.Error("HasComparison = true, want false — the old entry is outside both windows")
		}
	})

	t.Run("no data this week reports the empty state", func(t *testing.T) {
		got := buildWeeklySummary(nil, now)
		if got.Empty == "" {
			t.Error("Empty is blank, want an explanatory message")
		}
		if got.ThisWeekAvg != "" || got.HasComparison {
			t.Errorf("got %+v, want everything else zeroed", got)
		}
	})

	t.Run("only stale data also reports the empty state", func(t *testing.T) {
		entries := []db.Entry{entry(1, at(t, "2026-08-08 07:00"), 83.0, "")}
		got := buildWeeklySummary(entries, now)
		if got.Empty == "" {
			t.Error("Empty is blank, want the empty state when only last week has data")
		}
	})
}

func TestMean(t *testing.T) {
	if got := mean([]float64{80, 82, 84}); !nearlyEqual(got, 82) {
		t.Errorf("mean = %v, want 82", got)
	}
	if got := mean([]float64{81.5}); !nearlyEqual(got, 81.5) {
		t.Errorf("mean = %v, want 81.5", got)
	}
}
