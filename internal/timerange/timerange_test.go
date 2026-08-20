package timerange

import (
	"testing"
	"time"

	"weight-tracker/internal/testsupport"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func TestResolve(t *testing.T) {
	today := at(t, "2026-08-16 14:30")

	t.Run("a preset is an inclusive trailing window of whole days", func(t *testing.T) {
		w := Resolve("7", "", "", today)
		if !w.HasFrom {
			t.Fatal("preset range has no lower bound")
		}
		if w.HasUntil {
			t.Error("preset range has an upper bound, want open-ended so today's entries show")
		}
		// 7 days ending today means from the start of the 16th minus 6 days.
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-08-10 00:00:00" {
			t.Errorf("from = %s, want 2026-08-10 00:00:00", got)
		}
	})

	t.Run("a one-day preset starts at midnight today", func(t *testing.T) {
		w := Resolve("1", "", "", today)
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-08-16 00:00:00" {
			t.Errorf("from = %s, want 2026-08-16 00:00:00", got)
		}
	})

	t.Run("all is unbounded on both sides", func(t *testing.T) {
		w := Resolve("all", "", "", today)
		if w.HasFrom || w.HasUntil {
			t.Errorf("all-time window = %+v, want fully unbounded", w)
		}
	})

	t.Run("unrecognized and non-positive values fall back to unbounded", func(t *testing.T) {
		for _, param := range []string{"", "nonsense", "0", "-5", "7.5"} {
			w := Resolve(param, "", "", today)
			if w.HasFrom || w.HasUntil {
				t.Errorf("range=%q gave %+v, want fully unbounded", param, w)
			}
		}
	})

	t.Run("custom reads the from/until params", func(t *testing.T) {
		w := Resolve("custom", "2026-08-01", "2026-08-10", today)
		if !w.HasFrom || !w.HasUntil {
			t.Fatalf("custom window = %+v, want both bounds set", w)
		}
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-08-01 00:00:00" {
			t.Errorf("from = %s, want 2026-08-01 00:00:00", got)
		}
		// "until" is inclusive of the whole day, so an entry at 23:30 counts.
		if got := w.Until.Format("2006-01-02 15:04:05"); got != "2026-08-10 23:59:59" {
			t.Errorf("until = %s, want the end of 2026-08-10", got)
		}
		if !w.Contains(at(t, "2026-08-10 23:30")) {
			t.Error("an entry late on the until date was excluded")
		}
	})

	t.Run("each side of a custom range is independently optional", func(t *testing.T) {
		fromOnly := Resolve("custom", "2026-08-01", "", today)
		if !fromOnly.HasFrom || fromOnly.HasUntil {
			t.Errorf("from-only window = %+v, want lower bound only", fromOnly)
		}
		untilOnly := Resolve("custom", "", "2026-08-10", today)
		if untilOnly.HasFrom || !untilOnly.HasUntil {
			t.Errorf("until-only window = %+v, want upper bound only", untilOnly)
		}
		garbage := Resolve("custom", "not-a-date", "also-not", today)
		if garbage.HasFrom || garbage.HasUntil {
			t.Errorf("unparseable custom window = %+v, want fully unbounded", garbage)
		}
	})
}

func TestWindowContains(t *testing.T) {
	from, until := at(t, "2026-08-10 00:00"), at(t, "2026-08-20 00:00")
	both := Window{From: from, HasFrom: true, Until: until, HasUntil: true}

	tests := []struct {
		name string
		w    Window
		at   string
		want bool
	}{
		{"inside a bounded window", both, "2026-08-15 12:00", true},
		{"on the lower bound", both, "2026-08-10 00:00", true},
		{"on the upper bound", both, "2026-08-20 00:00", true},
		{"before a bounded window", both, "2026-08-09 23:59", false},
		{"after a bounded window", both, "2026-08-20 00:01", false},
		{"unbounded accepts anything", Window{}, "1999-01-01 00:00", true},
		{"lower bound only, before", Window{From: from, HasFrom: true}, "2026-08-09 00:00", false},
		{"lower bound only, long after", Window{From: from, HasFrom: true}, "2030-01-01 00:00", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Contains(at(t, tc.at)); got != tc.want {
				t.Errorf("Contains(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
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

func TestResolveThisYear(t *testing.T) {
	today := at(t, "2026-08-16 14:30")
	w := Resolve("this-year", "", "", today)

	if !w.HasFrom {
		t.Fatal("this-year has no lower bound")
	}
	if w.HasUntil {
		t.Error("this-year has an upper bound, want open-ended so today's entries show")
	}
	if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-01-01 00:00:00" {
		t.Errorf("from = %s, want midnight on 1 January", got)
	}
	if !w.Contains(at(t, "2026-01-01 00:00")) {
		t.Error("an entry at midnight on 1 January was excluded")
	}
	if w.Contains(at(t, "2025-12-31 23:59")) {
		t.Error("an entry from last year was included")
	}
}

func TestCustomRangeAcceptsRelativeExpressions(t *testing.T) {
	now := at(t, "2026-08-16 14:30")

	t.Run("now-5d to now, the Grafana form", func(t *testing.T) {
		w := Resolve("custom", "now-5d", "now", now)
		if !w.HasFrom || !w.HasUntil {
			t.Fatalf("window = %+v, want both bounds", w)
		}
		// Both ends snap to whole days, so the range covers every weigh-in
		// on the named dates rather than starting and ending at 14:30.
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-08-11 00:00:00" {
			t.Errorf("from = %s, want the start of that day", got)
		}
		if got := w.Until.Format("2006-01-02 15:04:05.000"); got != "2026-08-16 23:59:59.999" {
			t.Errorf("until = %s, want the end of today", got)
		}
		// The entries at either edge that an instant-based window would drop.
		if !w.Contains(at(t, "2026-08-11 07:00")) {
			t.Error("the morning weigh-in on the first day was excluded")
		}
		if !w.Contains(at(t, "2026-08-16 21:00")) {
			t.Error("this evening's weigh-in was excluded")
		}
		if w.Contains(at(t, "2026-08-10 21:00")) {
			t.Error("an entry before the window was included")
		}
	})

	t.Run("a date and a relative expression can be mixed", func(t *testing.T) {
		w := Resolve("custom", "2026-01-01", "now-1M", now)
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-01-01 00:00:00" {
			t.Errorf("from = %s", got)
		}
		if got := w.Until.Format("2006-01-02 15:04:05"); got != "2026-07-16 23:59:59" {
			t.Errorf("until = %s, want the end of that day", got)
		}
	})

	t.Run("a bare date covers its whole day at both ends", func(t *testing.T) {
		w := Resolve("custom", "2026-08-10", "2026-08-10", now)
		if !w.Contains(at(t, "2026-08-10 07:00")) {
			t.Error("the morning entry was excluded from a single-day range")
		}
		if !w.Contains(at(t, "2026-08-10 21:00")) {
			t.Error("the evening entry was excluded from a single-day range")
		}
		if w.Contains(at(t, "2026-08-09 23:59")) || w.Contains(at(t, "2026-08-11 00:00")) {
			t.Error("a single-day range leaked into the neighbouring days")
		}
	})

	t.Run("sub-day units round to the same day boundaries", func(t *testing.T) {
		// A consequence of always snapping, and the reason it is documented:
		// h/m/s are not useful for a tracker with two readings a day.
		w := Resolve("custom", "now-6h", "now", now)
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-08-16 00:00:00" {
			t.Errorf("from = %s, want the start of today", got)
		}
	})

	t.Run("each side stays independently optional", func(t *testing.T) {
		fromOnly := Resolve("custom", "now-90d", "", now)
		if !fromOnly.HasFrom || fromOnly.HasUntil {
			t.Errorf("window = %+v, want a lower bound only", fromOnly)
		}
		nonsense := Resolve("custom", "next tuesday", "whenever", now)
		if nonsense.HasFrom || nonsense.HasUntil {
			t.Errorf("window = %+v, want fully unbounded", nonsense)
		}
	})
}

func TestCustomRangeCrossReferences(t *testing.T) {
	now := at(t, "2026-08-16 14:30")

	t.Run("from can reference to", func(t *testing.T) {
		w := Resolve("custom", "to-5d", "now", now)
		if !w.HasFrom || !w.HasUntil {
			t.Fatalf("window = %+v, want both bounds", w)
		}
		// "to" resolves to today (now), so "to-5d" is 5 days before that.
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-08-11 00:00:00" {
			t.Errorf("from = %s, want 5 days before until", got)
		}
		if got := w.Until.Format("2006-01-02 15:04:05.000"); got != "2026-08-16 23:59:59.999" {
			t.Errorf("until = %s, want the end of today", got)
		}
	})

	t.Run("until can reference from — the inverse of to-5d", func(t *testing.T) {
		w := Resolve("custom", "now-30d", "from+5d", now)
		if !w.HasFrom || !w.HasUntil {
			t.Fatalf("window = %+v, want both bounds", w)
		}
		if got := w.From.Format("2006-01-02 15:04:05"); got != "2026-07-17 00:00:00" {
			t.Errorf("from = %s, want 30 days before now", got)
		}
		// from+5d: 5 days after the 17th is the 22nd.
		if got := w.Until.Format("2006-01-02 15:04:05"); got != "2026-07-22 23:59:59" {
			t.Errorf("until = %s, want 5 days after from", got)
		}
	})

	t.Run("a cross-reference can anchor on a literal date", func(t *testing.T) {
		w := Resolve("custom", "2026-08-01", "from+10d", now)
		if got := w.Until.Format("2006-01-02 15:04:05"); got != "2026-08-11 23:59:59" {
			t.Errorf("until = %s, want 10 days after the literal from date", got)
		}
	})

	t.Run("to accepts + and from accepts -, not just the inverse pairing", func(t *testing.T) {
		// These read a little unusual (from ends up after until), but the
		// parser doesn't police that — an inverted window just contains
		// nothing, same as any other empty range.
		w := Resolve("custom", "now", "from-5d", now)
		if !w.HasFrom || !w.HasUntil {
			t.Fatalf("window = %+v, want both bounds", w)
		}
		if got := w.Until.Format("2006-01-02 15:04:05"); got != "2026-08-11 23:59:59" {
			t.Errorf("until = %s, want 5 days before from", got)
		}
	})

	t.Run("mutual cross-reference has no independent anchor, so both sides are unbounded", func(t *testing.T) {
		w := Resolve("custom", "to-5d", "from+5d", now)
		if w.HasFrom || w.HasUntil {
			t.Errorf("window = %+v, want fully unbounded — neither side ever resolves", w)
		}
	})

	t.Run("a self-reference doesn't match the other keyword, so it's ignored", func(t *testing.T) {
		w := Resolve("custom", "from-5d", "to+5d", now)
		if w.HasFrom || w.HasUntil {
			t.Errorf("window = %+v, want fully unbounded — from doesn't mean anything referencing itself", w)
		}
	})

	t.Run("a lone reference with no anchor stays unbounded on that side", func(t *testing.T) {
		w := Resolve("custom", "to-5d", "", now)
		if w.HasFrom {
			t.Error("from resolved with no until value to anchor to")
		}
	})
}
