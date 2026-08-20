package summary

import (
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/testsupport"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func entry(id int64, recordedAt time.Time, weightKg float64, override string) db.Entry {
	return testsupport.Entry(id, recordedAt, weightKg, override)
}

func nearlyEqual(a, b float64) bool { return testsupport.NearlyEqual(a, b) }

func TestBuild(t *testing.T) {
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
		got := Build(entries, now)
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
		got := Build(entries, now)
		if got.ThisWeekAvg != "82.0 kg" {
			t.Errorf("ThisWeekAvg = %q, want the morning entry alone (82.0 kg)", got.ThisWeekAvg)
		}
	})

	t.Run("respects a period override when selecting entries", func(t *testing.T) {
		entries := []db.Entry{
			// Auto-detects as evening, overridden to morning, so it counts.
			entry(1, at(t, "2026-08-15 21:00"), 82.0, "morning"),
		}
		got := Build(entries, now)
		if got.ThisWeekAvg != "82.0 kg" {
			t.Errorf("ThisWeekAvg = %q, want the overridden entry to count", got.ThisWeekAvg)
		}
	})

	t.Run("reports a gain with a plus sign", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 07:00"), 83.0, ""),
			entry(2, at(t, "2026-08-08 07:00"), 82.0, ""),
		}
		got := Build(entries, now)
		if got.Delta != "+1.0 kg" {
			t.Errorf("Delta = %q, want %q", got.Delta, "+1.0 kg")
		}
		if got.DeltaIsLoss {
			t.Error("DeltaIsLoss = true for a gain")
		}
	})

	t.Run("no comparison when last week has no morning entries", func(t *testing.T) {
		entries := []db.Entry{entry(1, at(t, "2026-08-15 07:00"), 82.0, "")}
		got := Build(entries, now)
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
		got := Build(entries, now)
		if got.HasComparison {
			t.Error("HasComparison = true, want false — the old entry is outside both windows")
		}
	})

	t.Run("no data this week reports the empty state", func(t *testing.T) {
		got := Build(nil, now)
		if got.Empty == "" {
			t.Error("Empty is blank, want an explanatory message")
		}
		if got.ThisWeekAvg != "" || got.HasComparison {
			t.Errorf("got %+v, want everything else zeroed", got)
		}
	})

	t.Run("only stale data also reports the empty state", func(t *testing.T) {
		entries := []db.Entry{entry(1, at(t, "2026-08-08 07:00"), 83.0, "")}
		got := Build(entries, now)
		if got.Empty == "" {
			t.Error("Empty is blank, want the empty state when only last week has data")
		}
	})
}

func TestMeanKg(t *testing.T) {
	if got := meanKg([]int64{80000, 82000, 84000}); !nearlyEqual(got, 82) {
		t.Errorf("meanKg = %v, want 82", got)
	}
	if got := meanKg([]int64{81500}); !nearlyEqual(got, 81.5) {
		t.Errorf("meanKg = %v, want 81.5", got)
	}
	// Summing in integer grams keeps a mean that kilogram floats would drift
	// on: three readings that do not divide evenly still average exactly.
	if got := meanKg([]int64{80001, 80001, 80001}); !nearlyEqual(got, 80.001) {
		t.Errorf("meanKg = %v, want 80.001", got)
	}
}
