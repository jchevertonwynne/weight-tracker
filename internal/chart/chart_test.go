package chart

import (
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/testsupport"
	"weight-tracker/internal/timerange"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func entry(id int64, recordedAt time.Time, weightKg float64, override string) db.Entry {
	return testsupport.Entry(id, recordedAt, weightKg, override)
}

func nearlyEqual(a, b float64) bool { return testsupport.NearlyEqual(a, b) }

func TestRollingTrend(t *testing.T) {
	t.Run("averages over elapsed days, not sample count", func(t *testing.T) {
		// Four daily points; with a 7-day window every point averages all
		// preceding points, so the trend is a running mean.
		series := []rawPoint{
			{x: 0, val: 80},
			{x: 1, val: 82},
			{x: 2, val: 84},
			{x: 3, val: 86},
		}
		got := rollingTrend(series, 7)
		want := []float64{80, 81, 82, 83}
		if len(got) != len(want) {
			t.Fatalf("got %d trend points, want %d", len(got), len(want))
		}
		for i := range want {
			if !nearlyEqual(got[i].val, want[i]) {
				t.Errorf("trend[%d] = %v, want %v", i, got[i].val, want[i])
			}
		}
	})

	t.Run("points older than the window drop out", func(t *testing.T) {
		// A 2-day window: by x=10 the early cluster is long gone.
		series := []rawPoint{
			{x: 0, val: 100},
			{x: 1, val: 100},
			{x: 9, val: 80},
			{x: 10, val: 82},
		}
		got := rollingTrend(series, 2)
		if !nearlyEqual(got[2].val, 80) {
			t.Errorf("trend at x=9 = %v, want 80 (the 100s are outside the window)", got[2].val)
		}
		if !nearlyEqual(got[3].val, 81) {
			t.Errorf("trend at x=10 = %v, want 81 (mean of 80 and 82)", got[3].val)
		}
	})

	t.Run("several readings on the same day all count", func(t *testing.T) {
		series := []rawPoint{
			{x: 0, val: 80},
			{x: 0, val: 84},
		}
		got := rollingTrend(series, 7)
		if !nearlyEqual(got[1].val, 82) {
			t.Errorf("trend = %v, want 82", got[1].val)
		}
	})

	t.Run("preserves x and t alongside the smoothed value", func(t *testing.T) {
		ts := at(t, "2026-08-16 07:00")
		got := rollingTrend([]rawPoint{{x: 5, t: ts, val: 80}}, 7)
		if got[0].x != 5 || !got[0].t.Equal(ts) {
			t.Errorf("trend point = %+v, want x=5 and the original timestamp", got[0])
		}
	})

	t.Run("empty input yields empty output", func(t *testing.T) {
		if got := rollingTrend(nil, 7); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func TestFilterByWindow(t *testing.T) {
	pts := []rawPoint{
		{t: at(t, "2026-08-01 07:00"), val: 84},
		{t: at(t, "2026-08-10 07:00"), val: 83},
		{t: at(t, "2026-08-20 07:00"), val: 82},
	}

	t.Run("an unbounded window is a pass-through", func(t *testing.T) {
		got := filterByWindow(pts, timerange.Window{})
		if len(got) != 3 {
			t.Errorf("got %d points, want all 3", len(got))
		}
	})

	t.Run("trims to the visible range", func(t *testing.T) {
		w := timerange.Window{From: at(t, "2026-08-05 00:00"), HasFrom: true}
		got := filterByWindow(pts, w)
		if len(got) != 2 {
			t.Fatalf("got %d points, want 2", len(got))
		}
		if !nearlyEqual(got[0].val, 83) {
			t.Errorf("first kept point = %v, want 83", got[0].val)
		}
	})

	t.Run("no overlap yields nothing", func(t *testing.T) {
		w := timerange.Window{From: at(t, "2027-01-01 00:00"), HasFrom: true}
		if got := filterByWindow(pts, w); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func TestSequentialDeltas(t *testing.T) {
	chrono := []db.Entry{
		entry(1, at(t, "2026-08-14 07:00"), 84.0, ""),
		entry(2, at(t, "2026-08-14 21:00"), 85.0, ""),
		entry(3, at(t, "2026-08-16 07:00"), 83.0, ""), // skips the 15th entirely
		entry(4, at(t, "2026-08-16 21:00"), 84.5, ""),
	}

	t.Run("compares against the previous entry of the same period", func(t *testing.T) {
		got := sequentialDeltas(chrono, "morning")
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one delta", got)
		}
		// Morning-to-morning across a skipped day still compares.
		if got[3] != -1000 {
			t.Errorf("morning delta = %v g, want -1000", got[3])
		}
	})

	t.Run("evening series is independent of the morning one", func(t *testing.T) {
		got := sequentialDeltas(chrono, "evening")
		if got[4] != -500 {
			t.Errorf("evening delta = %v g, want -500", got[4])
		}
	})

	t.Run("the first entry of a period has no delta", func(t *testing.T) {
		got := sequentialDeltas(chrono, "morning")
		if _, ok := got[1]; ok {
			t.Error("the first morning entry got a delta, want none")
		}
	})
}

func TestBuild(t *testing.T) {
	today := at(t, "2026-08-16 23:00")
	entries := []db.Entry{
		entry(1, at(t, "2026-08-14 07:00"), 84.0, ""),
		entry(2, at(t, "2026-08-14 21:00"), 85.0, ""),
		entry(3, at(t, "2026-08-15 07:00"), 83.5, ""),
		entry(4, at(t, "2026-08-15 21:00"), 84.2, ""),
		entry(5, at(t, "2026-08-16 07:00"), 83.0, ""),
	}

	t.Run("all series plots every entry", func(t *testing.T) {
		got := Build(entries, nil, nil, "30", "all", "", "", today)
		if !got.HasData {
			t.Fatal("HasData = false, want true")
		}
		if len(got.Points) != 5 {
			t.Errorf("got %d points, want 5", len(got.Points))
		}
		if got.IsBar {
			t.Error("IsBar = true, want a line chart for the all series")
		}
		if got.XMin != entries[0].RecordedAt.UnixMilli() {
			t.Errorf("XMin = %d, want the first entry's timestamp", got.XMin)
		}
		if got.XMax != entries[4].RecordedAt.UnixMilli() {
			t.Errorf("XMax = %d, want the last entry's timestamp", got.XMax)
		}
	})

	t.Run("morning series filters by period and labels points", func(t *testing.T) {
		got := Build(entries, nil, nil, "30", "morning", "", "", today)
		if len(got.Points) != 3 {
			t.Fatalf("got %d points, want 3 morning entries", len(got.Points))
		}
		for _, p := range got.Points {
			if p.Color != "morning" {
				t.Errorf("point color = %q, want morning", p.Color)
			}
		}
		if got.Points[0].Value != "84.0 kg" {
			t.Errorf("value label = %q, want %q", got.Points[0].Value, "84.0 kg")
		}
		if got.Points[0].Date != "Aug 14" {
			t.Errorf("date label = %q, want %q", got.Points[0].Date, "Aug 14")
		}
	})

	t.Run("delta series is a bar chart with signed labels and no trend", func(t *testing.T) {
		got := Build(entries, nil, nil, "30", "morning-delta", "", "", today)
		if !got.IsBar {
			t.Error("IsBar = false, want true for a delta series")
		}
		if len(got.Points) != 2 {
			t.Fatalf("got %d bars, want 2 (the first morning has no predecessor)", len(got.Points))
		}
		if got.Points[0].Value != "-0.5 kg" {
			t.Errorf("bar label = %q, want %q", got.Points[0].Value, "-0.5 kg")
		}
		if got.Points[0].Color != "loss" {
			t.Errorf("bar color = %q, want loss", got.Points[0].Color)
		}
		if len(got.Trend) != 0 {
			t.Errorf("delta chart has a trend line (%d points), want none", len(got.Trend))
		}
	})

	t.Run("goal lines are omitted from delta charts but present on value charts", func(t *testing.T) {
		goals := []db.Goal{{ID: 1, WeightG: 80000, EffectiveFrom: at(t, "2026-08-01 00:00")}}
		value := Build(entries, goals, nil, "30", "all", "", "", today)
		if len(value.Goals) == 0 {
			t.Error("value chart has no goal line, want one")
		}
		delta := Build(entries, goals, nil, "30", "morning-delta", "", "", today)
		if len(delta.Goals) != 0 {
			t.Errorf("delta chart has %d goal points, want none", len(delta.Goals))
		}
	})

	// The trend is computed over full history and only then trimmed, so the
	// first visible point already carries the smoothing from data before it
	// rather than restarting at its own raw value. The "all" series splits
	// it per period, so morning and evening are each smoothed only over
	// their own readings.
	t.Run("trend is smoothed using data from before the visible range", func(t *testing.T) {
		// Visible range starts on the 15th, so the 14th's two readings are
		// off-chart but must still feed the first visible trend value.
		got := Build(entries, nil, nil, "custom", "all", "2026-08-15", "", today)
		if len(got.Points) != 3 {
			t.Fatalf("got %d visible points, want 3", len(got.Points))
		}
		// Morning source (full history): Aug14 84.0, Aug15 83.5, Aug16 83.0;
		// the 15th and 16th are visible.
		if len(got.TrendMorning) != 2 {
			t.Fatalf("got %d morning trend points, want 2", len(got.TrendMorning))
		}
		// The first visible point averages in the off-chart 14th rather than
		// restarting at its own raw 83.5.
		wantFirst := (84.0 + 83.5) / 2
		if !nearlyEqual(got.TrendMorning[0].Y, wantFirst) {
			t.Errorf("first morning trend point = %v, want %v (smoothed across off-chart history)", got.TrendMorning[0].Y, wantFirst)
		}
		if nearlyEqual(got.TrendMorning[0].Y, 83.5) {
			t.Error("first morning trend point equals its own raw value — the window was truncated at the range edge")
		}
		// Evening source (full history) is just Aug14 and Aug15; the 14th is
		// off-chart, leaving a single visible evening point — not enough to
		// draw a trend line.
		if len(got.TrendEvening) != 0 {
			t.Errorf("got %d evening trend points, want 0 (only one evening reading is visible)", len(got.TrendEvening))
		}
	})

	t.Run("a single visible point produces no trend line", func(t *testing.T) {
		// A one-point line conveys nothing, so the trend is suppressed.
		got := Build(entries, nil, nil, "custom", "all", "2026-08-16", "", today)
		if len(got.Points) != 1 {
			t.Fatalf("got %d visible points, want 1", len(got.Points))
		}
		if len(got.TrendMorning) != 0 {
			t.Errorf("got %d morning trend points, want none", len(got.TrendMorning))
		}
		if len(got.TrendEvening) != 0 {
			t.Errorf("got %d evening trend points, want none", len(got.TrendEvening))
		}
	})

	t.Run("a single point produces no trend line", func(t *testing.T) {
		got := Build(entries[:1], nil, nil, "30", "all", "", "", today)
		if len(got.TrendMorning) != 0 {
			t.Errorf("got %d morning trend points for a single entry, want none", len(got.TrendMorning))
		}
		if len(got.TrendEvening) != 0 {
			t.Errorf("got %d evening trend points for a single entry, want none", len(got.TrendEvening))
		}
	})

	t.Run("an empty range reports why", func(t *testing.T) {
		got := Build(entries, nil, nil, "custom", "all", "2027-01-01", "2027-02-01", today)
		if got.HasData {
			t.Error("HasData = true for an empty range")
		}
		if got.Empty == "" {
			t.Error("Empty message is blank, want an explanation")
		}
		if len(got.Points) != 0 {
			t.Errorf("got %d points, want none", len(got.Points))
		}
	})

	t.Run("empty-state message is specific to the series", func(t *testing.T) {
		got := Build(entries[:1], nil, nil, "30", "morning-delta", "", "", today)
		if got.HasData {
			t.Fatal("HasData = true, want false with only one morning entry")
		}
		if got.Empty == emptyMessage("all") {
			t.Errorf("delta series got the generic message %q", got.Empty)
		}
	})

	t.Run("no entries at all", func(t *testing.T) {
		got := Build(nil, nil, nil, "30", "all", "", "", today)
		if got.HasData || got.Empty == "" {
			t.Errorf("got %+v, want an empty result with a message", got)
		}
	})
}

func TestDeltaClass(t *testing.T) {
	if got := deltaClass(-100); got != "loss" {
		t.Errorf("deltaClass(-100g) = %q, want loss", got)
	}
	if got := deltaClass(100); got != "gain" {
		t.Errorf("deltaClass(100g) = %q, want gain", got)
	}
	if got := deltaClass(0); got != "gain" {
		t.Errorf("deltaClass(0) = %q, want gain (no change is not a loss)", got)
	}
}

func TestDayNumTracksElapsedTime(t *testing.T) {
	a := at(t, "2026-08-15 07:00")
	b := a.Add(24 * time.Hour)
	if diff := dayNum(b) - dayNum(a); !nearlyEqual(diff, 1) {
		t.Errorf("24h apart = %v days, want 1", diff)
	}
	half := a.Add(12 * time.Hour)
	if diff := dayNum(half) - dayNum(a); !nearlyEqual(diff, 0.5) {
		t.Errorf("12h apart = %v days, want 0.5", diff)
	}
}
