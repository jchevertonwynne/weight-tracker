package weight

import (
	"strconv"
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/testsupport"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func entry(id int64, recordedAt time.Time, weightKg float64, override string) db.Entry {
	return testsupport.Entry(id, recordedAt, weightKg, override)
}

func TestEntryPeriod(t *testing.T) {
	tests := []struct {
		name     string
		at       string
		override string
		want     string
	}{
		{"mid-morning auto-detects morning", "2026-08-16 07:30", "", "morning"},
		{"4am is the first morning minute", "2026-08-16 04:00", "", "morning"},
		{"3:59am is still last night", "2026-08-16 03:59", "", "evening"},
		{"noon flips to evening", "2026-08-16 12:00", "", "evening"},
		{"11:59am is still morning", "2026-08-16 11:59", "", "morning"},
		{"override wins over detection", "2026-08-16 07:30", "evening", "evening"},
		{"override wins the other way too", "2026-08-16 21:00", "morning", "morning"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EntryPeriod(entry(1, at(t, tc.at), 80, tc.override))
			if got != tc.want {
				t.Errorf("EntryPeriod(%s, override=%q) = %q, want %q", tc.at, tc.override, got, tc.want)
			}
		})
	}
}

func TestValidPeriodOverride(t *testing.T) {
	for _, valid := range []string{"", "morning", "evening"} {
		if !ValidPeriodOverride(valid) {
			t.Errorf("ValidPeriodOverride(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"noon", "Morning", "afternoon", " "} {
		if ValidPeriodOverride(invalid) {
			t.Errorf("ValidPeriodOverride(%q) = true, want false", invalid)
		}
	}
}

func TestLogicalDate(t *testing.T) {
	tests := []struct {
		name string
		at   string
		want string
	}{
		{"midday belongs to its own date", "2026-08-16 12:30", "2026-08-16"},
		{"late evening belongs to its own date", "2026-08-16 23:45", "2026-08-16"},
		{"1am belongs to the previous date", "2026-08-16 01:25", "2026-08-15"},
		{"3:59am still belongs to the previous date", "2026-08-16 03:59", "2026-08-15"},
		{"4am starts the new date", "2026-08-16 04:00", "2026-08-16"},
		{"pre-4am across a month boundary", "2026-09-01 02:00", "2026-08-31"},
		{"pre-4am across a year boundary", "2026-01-01 00:30", "2025-12-31"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := logicalDate(at(t, tc.at)).Format("2006-01-02")
			if got != tc.want {
				t.Errorf("logicalDate(%s) = %s, want %s", tc.at, got, tc.want)
			}
		})
	}
}

func TestSameDateAndNextCalendarDay(t *testing.T) {
	tests := []struct {
		name        string
		a, b        string
		wantSame    bool
		wantNextDay bool
	}{
		{"same day, morning then evening", "2026-08-16 07:00", "2026-08-16 21:00", true, false},
		{"1am counts as the previous day's evening", "2026-08-16 01:25", "2026-08-15 22:00", true, false},
		{"consecutive days", "2026-08-15 21:00", "2026-08-16 07:00", false, true},
		{"1:25am evening then that morning is consecutive", "2026-08-16 01:25", "2026-08-16 07:15", false, true},
		{"two-day gap is neither", "2026-08-14 21:00", "2026-08-16 07:00", false, false},
		{"reversed order is not next-day", "2026-08-16 07:00", "2026-08-15 21:00", false, false},
		{"month boundary is consecutive", "2026-08-31 21:00", "2026-09-01 07:00", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := at(t, tc.a), at(t, tc.b)
			if got := sameDate(a, b); got != tc.wantSame {
				t.Errorf("sameDate(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.wantSame)
			}
			if got := isNextCalendarDay(a, b); got != tc.wantNextDay {
				t.Errorf("isNextCalendarDay(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.wantNextDay)
			}
		})
	}
}

func TestChronologicalWithDeltas(t *testing.T) {
	t.Run("sorts ascending regardless of input order", func(t *testing.T) {
		entries := []db.Entry{
			entry(3, at(t, "2026-08-16 07:15"), 82.0, ""),
			entry(1, at(t, "2026-08-15 07:30"), 82.4, ""),
			entry(2, at(t, "2026-08-15 21:00"), 83.1, ""),
		}
		chrono, _, _ := ChronologicalWithDeltas(entries)
		var gotIDs []int64
		for _, e := range chrono {
			gotIDs = append(gotIDs, e.ID)
		}
		want := []int64{1, 2, 3}
		for i := range want {
			if gotIDs[i] != want[i] {
				t.Fatalf("chronological order = %v, want %v", gotIDs, want)
			}
		}
	})

	t.Run("does not mutate the caller's slice", func(t *testing.T) {
		entries := []db.Entry{
			entry(2, at(t, "2026-08-16 07:15"), 82.0, ""),
			entry(1, at(t, "2026-08-15 07:30"), 82.4, ""),
		}
		ChronologicalWithDeltas(entries)
		if entries[0].ID != 2 {
			t.Errorf("input slice was reordered: first ID = %d, want 2", entries[0].ID)
		}
	})

	t.Run("overnight delta pairs an evening with the next morning", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 21:00"), 83.1, ""),
			entry(2, at(t, "2026-08-16 07:15"), 82.0, ""),
		}
		_, overnight, daily := ChronologicalWithDeltas(entries)
		got, ok := overnight[2]
		if !ok {
			t.Fatal("no overnight delta for the morning entry")
		}
		if got != -1100 {
			t.Errorf("overnight delta = %v g, want -1100", got)
		}
		if len(daily) != 0 {
			t.Errorf("unexpected daily deltas: %v", daily)
		}
	})

	t.Run("daily delta pairs a morning with that same day's evening", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 07:30"), 82.4, ""),
			entry(2, at(t, "2026-08-15 21:00"), 83.1, ""),
		}
		_, overnight, daily := ChronologicalWithDeltas(entries)
		got, ok := daily[2]
		if !ok {
			t.Fatal("no daily delta for the evening entry")
		}
		if got != 700 {
			t.Errorf("daily delta = %v g, want 700", got)
		}
		if len(overnight) != 0 {
			t.Errorf("unexpected overnight deltas: %v", overnight)
		}
	})

	// Regression test for the fix in commit bcd0eef: a 1:25am weigh-in is
	// classified "evening" but shares its calendar date with the morning
	// that follows it, so a literal same-date comparison missed the pair.
	t.Run("late-night evening pairs with the morning hours later", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-16 01:25"), 83.5, ""),
			entry(2, at(t, "2026-08-16 07:15"), 82.6, ""),
		}
		_, overnight, _ := ChronologicalWithDeltas(entries)
		got, ok := overnight[2]
		if !ok {
			t.Fatal("no overnight delta across the 4am boundary")
		}
		if got != -900 {
			t.Errorf("overnight delta = %v g, want -900", got)
		}
	})

	t.Run("a gap suppresses the overnight delta", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-13 21:00"), 83.1, ""),
			entry(2, at(t, "2026-08-16 07:15"), 82.0, ""),
		}
		_, overnight, _ := ChronologicalWithDeltas(entries)
		if delta, ok := overnight[2]; ok {
			t.Errorf("got overnight delta %v across a two-day gap, want none", delta)
		}
	})

	t.Run("a gap suppresses the daily delta", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 07:30"), 82.4, ""),
			entry(2, at(t, "2026-08-16 21:00"), 83.1, ""),
		}
		_, _, daily := ChronologicalWithDeltas(entries)
		if delta, ok := daily[2]; ok {
			t.Errorf("got daily delta %v for a different day, want none", delta)
		}
	})

	t.Run("period override redirects the pairing", func(t *testing.T) {
		// The 09:00 entry would auto-detect as morning; overriding it to
		// evening makes it the partner for the next day's morning instead.
		entries := []db.Entry{
			entry(1, at(t, "2026-08-15 09:00"), 83.0, "evening"),
			entry(2, at(t, "2026-08-16 07:15"), 82.0, ""),
		}
		_, overnight, _ := ChronologicalWithDeltas(entries)
		got, ok := overnight[2]
		if !ok {
			t.Fatal("no overnight delta when the partner was overridden to evening")
		}
		if got != -1000 {
			t.Errorf("overnight delta = %v g, want -1000", got)
		}
	})

	t.Run("a full multi-day run pairs every adjacent reading", func(t *testing.T) {
		entries := []db.Entry{
			entry(1, at(t, "2026-08-14 07:00"), 84.0, ""),
			entry(2, at(t, "2026-08-14 21:00"), 85.0, ""),
			entry(3, at(t, "2026-08-15 07:00"), 83.5, ""),
			entry(4, at(t, "2026-08-15 21:00"), 84.2, ""),
			entry(5, at(t, "2026-08-16 07:00"), 83.0, ""),
		}
		_, overnight, daily := ChronologicalWithDeltas(entries)
		wantOvernight := map[int64]int64{3: -1500, 5: -1200}
		wantDaily := map[int64]int64{2: 1000, 4: 700}
		if len(overnight) != len(wantOvernight) {
			t.Errorf("overnight = %v, want %v", overnight, wantOvernight)
		}
		for id, want := range wantOvernight {
			if got := overnight[id]; got != want {
				t.Errorf("overnight[%d] = %v, want %v", id, got, want)
			}
		}
		if len(daily) != len(wantDaily) {
			t.Errorf("daily = %v, want %v", daily, wantDaily)
		}
		for id, want := range wantDaily {
			if got := daily[id]; got != want {
				t.Errorf("daily[%d] = %v, want %v", id, got, want)
			}
		}
	})

	t.Run("no entries yields empty maps", func(t *testing.T) {
		chrono, overnight, daily := ChronologicalWithDeltas(nil)
		if len(chrono) != 0 || len(overnight) != 0 || len(daily) != 0 {
			t.Errorf("got %v/%v/%v, want all empty", chrono, overnight, daily)
		}
	})
}

func TestFormatKg(t *testing.T) {
	tests := []struct {
		grams int64
		want  string
	}{
		{82400, "82.4"},
		{82000, "82.0"},
		{81647, "81.6"}, // display rounds to the scale's precision
		{81650, "81.7"},
		{0, "0.0"},
	}
	for _, tc := range tests {
		if got := FormatKg(tc.grams); got != tc.want {
			t.Errorf("FormatKg(%d) = %q, want %q", tc.grams, got, tc.want)
		}
	}
}

func TestFormatKgDelta(t *testing.T) {
	tests := []struct {
		grams int64
		want  string
	}{
		{-1100, "-1.1 kg"},
		{700, "+0.7 kg"},
		{0, "+0.0 kg"},
		{-50, "-0.1 kg"}, // rounds for display, but the sign is never lost
	}
	for _, tc := range tests {
		if got := FormatKgDelta(tc.grams); got != tc.want {
			t.Errorf("FormatKgDelta(%d) = %q, want %q", tc.grams, got, tc.want)
		}
	}
}

func TestFormatKgInputKeepsStoredPrecision(t *testing.T) {
	// The edit form must round-trip the stored value exactly, so that saving
	// an untouched row cannot quietly change it.
	tests := []struct {
		grams int64
		want  string
	}{
		{82400, "82.4"},
		{82000, "82"},
		{81647, "81.647"},
		{81600, "81.6"},
		{1, "0.001"},
	}
	for _, tc := range tests {
		got := FormatKgInput(tc.grams)
		if got != tc.want {
			t.Errorf("FormatKgInput(%d) = %q, want %q", tc.grams, got, tc.want)
		}
		// Whatever it renders must parse back to the same grams.
		if back := db.KgToGrams(mustParseFloat(t, got)); back != tc.grams {
			t.Errorf("FormatKgInput(%d) = %q, which reads back as %d g", tc.grams, got, back)
		}
	}
}

func mustParseFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return f
}
