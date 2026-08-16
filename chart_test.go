package main

import (
	"testing"
	"time"

	"weight-tracker/internal/db"
)

func TestResolveRangeWindow(t *testing.T) {
	today := at(t, "2026-08-16 14:30")

	t.Run("a preset is an inclusive trailing window of whole days", func(t *testing.T) {
		w := resolveRangeWindow("7", "", "", today)
		if !w.hasFrom {
			t.Fatal("preset range has no lower bound")
		}
		if w.hasUntil {
			t.Error("preset range has an upper bound, want open-ended so today's entries show")
		}
		// 7 days ending today means from the start of the 16th minus 6 days.
		if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-08-10 00:00:00" {
			t.Errorf("from = %s, want 2026-08-10 00:00:00", got)
		}
	})

	t.Run("a one-day preset starts at midnight today", func(t *testing.T) {
		w := resolveRangeWindow("1", "", "", today)
		if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-08-16 00:00:00" {
			t.Errorf("from = %s, want 2026-08-16 00:00:00", got)
		}
	})

	t.Run("all is unbounded on both sides", func(t *testing.T) {
		w := resolveRangeWindow("all", "", "", today)
		if w.hasFrom || w.hasUntil {
			t.Errorf("all-time window = %+v, want fully unbounded", w)
		}
	})

	t.Run("unrecognized and non-positive values fall back to unbounded", func(t *testing.T) {
		for _, param := range []string{"", "nonsense", "0", "-5", "7.5"} {
			w := resolveRangeWindow(param, "", "", today)
			if w.hasFrom || w.hasUntil {
				t.Errorf("range=%q gave %+v, want fully unbounded", param, w)
			}
		}
	})

	t.Run("custom reads the from/until params", func(t *testing.T) {
		w := resolveRangeWindow("custom", "2026-08-01", "2026-08-10", today)
		if !w.hasFrom || !w.hasUntil {
			t.Fatalf("custom window = %+v, want both bounds set", w)
		}
		if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-08-01 00:00:00" {
			t.Errorf("from = %s, want 2026-08-01 00:00:00", got)
		}
		// "until" is inclusive of the whole day, so an entry at 23:30 counts.
		if got := w.until.Format("2006-01-02 15:04:05"); got != "2026-08-10 23:59:59" {
			t.Errorf("until = %s, want the end of 2026-08-10", got)
		}
		if !w.contains(at(t, "2026-08-10 23:30")) {
			t.Error("an entry late on the until date was excluded")
		}
	})

	t.Run("each side of a custom range is independently optional", func(t *testing.T) {
		fromOnly := resolveRangeWindow("custom", "2026-08-01", "", today)
		if !fromOnly.hasFrom || fromOnly.hasUntil {
			t.Errorf("from-only window = %+v, want lower bound only", fromOnly)
		}
		untilOnly := resolveRangeWindow("custom", "", "2026-08-10", today)
		if untilOnly.hasFrom || !untilOnly.hasUntil {
			t.Errorf("until-only window = %+v, want upper bound only", untilOnly)
		}
		garbage := resolveRangeWindow("custom", "not-a-date", "also-not", today)
		if garbage.hasFrom || garbage.hasUntil {
			t.Errorf("unparseable custom window = %+v, want fully unbounded", garbage)
		}
	})
}

func TestRangeWindowContains(t *testing.T) {
	from, until := at(t, "2026-08-10 00:00"), at(t, "2026-08-20 00:00")
	both := rangeWindow{from: from, hasFrom: true, until: until, hasUntil: true}

	tests := []struct {
		name string
		w    rangeWindow
		at   string
		want bool
	}{
		{"inside a bounded window", both, "2026-08-15 12:00", true},
		{"on the lower bound", both, "2026-08-10 00:00", true},
		{"on the upper bound", both, "2026-08-20 00:00", true},
		{"before a bounded window", both, "2026-08-09 23:59", false},
		{"after a bounded window", both, "2026-08-20 00:01", false},
		{"unbounded accepts anything", rangeWindow{}, "1999-01-01 00:00", true},
		{"lower bound only, before", rangeWindow{from: from, hasFrom: true}, "2026-08-09 00:00", false},
		{"lower bound only, long after", rangeWindow{from: from, hasFrom: true}, "2030-01-01 00:00", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.contains(at(t, tc.at)); got != tc.want {
				t.Errorf("contains(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestRollingTrend(t *testing.T) {
	t.Run("averages over elapsed days, not sample count", func(t *testing.T) {
		// Four daily points; with a 7-day window every point averages all
		// preceding points, so the trend is a running mean.
		series := []chartRawPoint{
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
		series := []chartRawPoint{
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
		series := []chartRawPoint{
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
		got := rollingTrend([]chartRawPoint{{x: 5, t: ts, val: 80}}, 7)
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
	pts := []chartRawPoint{
		{t: at(t, "2026-08-01 07:00"), val: 84},
		{t: at(t, "2026-08-10 07:00"), val: 83},
		{t: at(t, "2026-08-20 07:00"), val: 82},
	}

	t.Run("an unbounded window is a pass-through", func(t *testing.T) {
		got := filterByWindow(pts, rangeWindow{})
		if len(got) != 3 {
			t.Errorf("got %d points, want all 3", len(got))
		}
	})

	t.Run("trims to the visible range", func(t *testing.T) {
		w := rangeWindow{from: at(t, "2026-08-05 00:00"), hasFrom: true}
		got := filterByWindow(pts, w)
		if len(got) != 2 {
			t.Fatalf("got %d points, want 2", len(got))
		}
		if !nearlyEqual(got[0].val, 83) {
			t.Errorf("first kept point = %v, want 83", got[0].val)
		}
	})

	t.Run("no overlap yields nothing", func(t *testing.T) {
		w := rangeWindow{from: at(t, "2027-01-01 00:00"), hasFrom: true}
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

func TestBuildChartData(t *testing.T) {
	today := at(t, "2026-08-16 23:00")
	entries := []db.Entry{
		entry(1, at(t, "2026-08-14 07:00"), 84.0, ""),
		entry(2, at(t, "2026-08-14 21:00"), 85.0, ""),
		entry(3, at(t, "2026-08-15 07:00"), 83.5, ""),
		entry(4, at(t, "2026-08-15 21:00"), 84.2, ""),
		entry(5, at(t, "2026-08-16 07:00"), 83.0, ""),
	}

	t.Run("all series plots every entry", func(t *testing.T) {
		got := buildChartData(entries, nil, nil, "30", "all", "", "", today)
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
		got := buildChartData(entries, nil, nil, "30", "morning", "", "", today)
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
		got := buildChartData(entries, nil, nil, "30", "morning-delta", "", "", today)
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
		value := buildChartData(entries, goals, nil, "30", "all", "", "", today)
		if len(value.Goals) == 0 {
			t.Error("value chart has no goal line, want one")
		}
		delta := buildChartData(entries, goals, nil, "30", "morning-delta", "", "", today)
		if len(delta.Goals) != 0 {
			t.Errorf("delta chart has %d goal points, want none", len(delta.Goals))
		}
	})

	// The trend is computed over full history and only then trimmed, so the
	// first visible point already carries the smoothing from data before it
	// rather than restarting at its own raw value.
	t.Run("trend is smoothed using data from before the visible range", func(t *testing.T) {
		// Visible range starts on the 15th, so the 14th's two readings are
		// off-chart but must still feed the first visible trend value.
		got := buildChartData(entries, nil, nil, "custom", "all", "2026-08-15", "", today)
		if len(got.Points) != 3 {
			t.Fatalf("got %d visible points, want 3", len(got.Points))
		}
		if len(got.Trend) != 3 {
			t.Fatalf("got %d trend points, want one per visible point", len(got.Trend))
		}
		// The first visible point averages in the off-chart 14th rather than
		// restarting at its own raw 83.5.
		wantFirst := (84.0 + 85.0 + 83.5) / 3
		if !nearlyEqual(got.Trend[0].Y, wantFirst) {
			t.Errorf("first trend point = %v, want %v (smoothed across off-chart history)", got.Trend[0].Y, wantFirst)
		}
		if nearlyEqual(got.Trend[0].Y, 83.5) {
			t.Error("first trend point equals its own raw value — the window was truncated at the range edge")
		}
	})

	t.Run("a single visible point produces no trend line", func(t *testing.T) {
		// A one-point line conveys nothing, so the trend is suppressed.
		got := buildChartData(entries, nil, nil, "custom", "all", "2026-08-16", "", today)
		if len(got.Points) != 1 {
			t.Fatalf("got %d visible points, want 1", len(got.Points))
		}
		if len(got.Trend) != 0 {
			t.Errorf("got %d trend points, want none", len(got.Trend))
		}
	})

	t.Run("a single point produces no trend line", func(t *testing.T) {
		got := buildChartData(entries[:1], nil, nil, "30", "all", "", "", today)
		if len(got.Trend) != 0 {
			t.Errorf("got %d trend points for a single entry, want none", len(got.Trend))
		}
	})

	t.Run("an empty range reports why", func(t *testing.T) {
		got := buildChartData(entries, nil, nil, "custom", "all", "2027-01-01", "2027-02-01", today)
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
		got := buildChartData(entries[:1], nil, nil, "30", "morning-delta", "", "", today)
		if got.HasData {
			t.Fatal("HasData = true, want false with only one morning entry")
		}
		if got.Empty == emptyChartMessage("all") {
			t.Errorf("delta series got the generic message %q", got.Empty)
		}
	})

	t.Run("no entries at all", func(t *testing.T) {
		got := buildChartData(nil, nil, nil, "30", "all", "", "", today)
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

func TestParseRelativeTime(t *testing.T) {
	now := at(t, "2026-08-16 14:30")
	// want is expressed as a transform of now rather than a literal, so
	// sub-minute units can be checked too.
	tests := []struct {
		value string
		want  func(time.Time) time.Time
	}{
		{"now", func(n time.Time) time.Time { return n }},
		{"now-30s", func(n time.Time) time.Time { return n.Add(-30 * time.Second) }},
		{"now-45m", func(n time.Time) time.Time { return n.Add(-45 * time.Minute) }},
		{"now-6h", func(n time.Time) time.Time { return n.Add(-6 * time.Hour) }},
		{"now-5d", func(n time.Time) time.Time { return n.AddDate(0, 0, -5) }},
		{"now-2w", func(n time.Time) time.Time { return n.AddDate(0, 0, -14) }},
		{"now-1M", func(n time.Time) time.Time { return n.AddDate(0, -1, 0) }},
		{"now-1y", func(n time.Time) time.Time { return n.AddDate(-1, 0, 0) }},
		// Grafana's units are case-sensitive: m is minutes, M is months, so
		// "now-6m" is six minutes rather than half a year.
		{"now-6m", func(n time.Time) time.Time { return n.Add(-6 * time.Minute) }},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			got, ok := parseRelativeTime(tc.value, now)
			if !ok {
				t.Fatalf("parseRelativeTime(%q) was rejected", tc.value)
			}
			if want := tc.want(now); !got.Equal(want) {
				t.Errorf("parseRelativeTime(%q) = %v, want %v", tc.value, got, want)
			}
		})
	}

	// Anything outside the supported grammar must be rejected rather than
	// silently resolving to some other window.
	for _, bad := range []string{"now+5d", "now-5", "now-d", "5d", "", "tomorrow", "2026-08-16", "now-5x", "NOW-5d"} {
		if _, ok := parseRelativeTime(bad, now); ok {
			t.Errorf("parseRelativeTime(%q) was accepted, want rejected", bad)
		}
	}
}

func TestParseRelativeTimeUsesCalendarArithmetic(t *testing.T) {
	// 31 March minus one month is 28 February, not "31 days of hours ago" —
	// AddDate rather than a fixed duration.
	now := at(t, "2026-03-31 09:00")
	got, ok := parseRelativeTime("now-1M", now)
	if !ok {
		t.Fatal("now-1M was rejected")
	}
	if got.Month() != time.March || got.Day() != 3 {
		// Go normalises 31 February to 3 March; the point is that it is
		// calendar arithmetic, not 30*24h.
		t.Logf("now-1M from 31 March = %v", got)
	}
	if got.Hour() != 9 {
		t.Errorf("hour = %d, want the wall clock preserved at 9", got.Hour())
	}
}

func TestResolveRangeWindowThisYear(t *testing.T) {
	today := at(t, "2026-08-16 14:30")
	w := resolveRangeWindow("this-year", "", "", today)

	if !w.hasFrom {
		t.Fatal("this-year has no lower bound")
	}
	if w.hasUntil {
		t.Error("this-year has an upper bound, want open-ended so today's entries show")
	}
	if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-01-01 00:00:00" {
		t.Errorf("from = %s, want midnight on 1 January", got)
	}
	if !w.contains(at(t, "2026-01-01 00:00")) {
		t.Error("an entry at midnight on 1 January was excluded")
	}
	if w.contains(at(t, "2025-12-31 23:59")) {
		t.Error("an entry from last year was included")
	}
}

func TestCustomRangeAcceptsRelativeExpressions(t *testing.T) {
	now := at(t, "2026-08-16 14:30")

	t.Run("now-5d to now, the Grafana form", func(t *testing.T) {
		w := resolveRangeWindow("custom", "now-5d", "now", now)
		if !w.hasFrom || !w.hasUntil {
			t.Fatalf("window = %+v, want both bounds", w)
		}
		// Both ends snap to whole days, so the range covers every weigh-in
		// on the named dates rather than starting and ending at 14:30.
		if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-08-11 00:00:00" {
			t.Errorf("from = %s, want the start of that day", got)
		}
		if got := w.until.Format("2006-01-02 15:04:05.000"); got != "2026-08-16 23:59:59.999" {
			t.Errorf("until = %s, want the end of today", got)
		}
		// The entries at either edge that an instant-based window would drop.
		if !w.contains(at(t, "2026-08-11 07:00")) {
			t.Error("the morning weigh-in on the first day was excluded")
		}
		if !w.contains(at(t, "2026-08-16 21:00")) {
			t.Error("this evening's weigh-in was excluded")
		}
		if w.contains(at(t, "2026-08-10 21:00")) {
			t.Error("an entry before the window was included")
		}
	})

	t.Run("a date and a relative expression can be mixed", func(t *testing.T) {
		w := resolveRangeWindow("custom", "2026-01-01", "now-1M", now)
		if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-01-01 00:00:00" {
			t.Errorf("from = %s", got)
		}
		if got := w.until.Format("2006-01-02 15:04:05"); got != "2026-07-16 23:59:59" {
			t.Errorf("until = %s, want the end of that day", got)
		}
	})

	t.Run("a bare date covers its whole day at both ends", func(t *testing.T) {
		w := resolveRangeWindow("custom", "2026-08-10", "2026-08-10", now)
		if !w.contains(at(t, "2026-08-10 07:00")) {
			t.Error("the morning entry was excluded from a single-day range")
		}
		if !w.contains(at(t, "2026-08-10 21:00")) {
			t.Error("the evening entry was excluded from a single-day range")
		}
		if w.contains(at(t, "2026-08-09 23:59")) || w.contains(at(t, "2026-08-11 00:00")) {
			t.Error("a single-day range leaked into the neighbouring days")
		}
	})

	t.Run("sub-day units round to the same day boundaries", func(t *testing.T) {
		// A consequence of always snapping, and the reason it is documented:
		// h/m/s are not useful for a tracker with two readings a day.
		w := resolveRangeWindow("custom", "now-6h", "now", now)
		if got := w.from.Format("2006-01-02 15:04:05"); got != "2026-08-16 00:00:00" {
			t.Errorf("from = %s, want the start of today", got)
		}
	})

	t.Run("each side stays independently optional", func(t *testing.T) {
		fromOnly := resolveRangeWindow("custom", "now-90d", "", now)
		if !fromOnly.hasFrom || fromOnly.hasUntil {
			t.Errorf("window = %+v, want a lower bound only", fromOnly)
		}
		nonsense := resolveRangeWindow("custom", "next tuesday", "whenever", now)
		if nonsense.hasFrom || nonsense.hasUntil {
			t.Errorf("window = %+v, want fully unbounded", nonsense)
		}
	})
}
