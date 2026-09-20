package history

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

func TestBuildRows(t *testing.T) {
	entries := []db.Entry{
		// Newest-first, as db.ListEntries returns them.
		entry(2, at(t, "2026-08-16 07:15"), 82.04, ""),
		entry(1, at(t, "2026-08-15 21:00"), 83.1, ""),
	}
	rows := BuildRows(entries, at(t, "2026-09-01 12:00"))
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	morning := rows[0]
	if morning.PeriodLabel != "Morning" || morning.Period != "morning" {
		t.Errorf("period = %q/%q, want morning/Morning", morning.Period, morning.PeriodLabel)
	}
	if morning.WeightKgStr != "82.0" {
		t.Errorf("WeightKgStr = %q, want %q (display rounds to 1dp)", morning.WeightKgStr, "82.0")
	}
	if morning.WeightKgRaw != "82.04" {
		t.Errorf("WeightKgRaw = %q, want %q (edit form keeps full precision)", morning.WeightKgRaw, "82.04")
	}
	if morning.RecordedAtDate != "2026-08-16" || morning.RecordedAtTime != "07:15" {
		t.Errorf("edit-form fields = %q/%q, want 2026-08-16/07:15", morning.RecordedAtDate, morning.RecordedAtTime)
	}
	if morning.RecordedAtLabel != "Aug 16, 2026 07:15" {
		t.Errorf("RecordedAtLabel = %q", morning.RecordedAtLabel)
	}
	if morning.OvernightDelta != "-1.1 kg" {
		t.Errorf("OvernightDelta = %q, want %q", morning.OvernightDelta, "-1.1 kg")
	}
	if !morning.OvernightLoss {
		t.Error("OvernightLoss = false, want true for a -1.1 kg change")
	}

	evening := rows[1]
	if evening.PeriodLabel != "Evening" {
		t.Errorf("PeriodLabel = %q, want Evening", evening.PeriodLabel)
	}
	if evening.OvernightDelta != "" {
		t.Errorf("evening row has an overnight delta %q, want none", evening.OvernightDelta)
	}
}

func TestBuildRowsFormatsGainsWithASign(t *testing.T) {
	entries := []db.Entry{
		entry(2, at(t, "2026-08-15 21:00"), 83.1, ""),
		entry(1, at(t, "2026-08-15 07:30"), 82.4, ""),
	}
	rows := BuildRows(entries, at(t, "2026-09-01 12:00"))
	evening := rows[0]
	if evening.DailyDelta != "+0.7 kg" {
		t.Errorf("DailyDelta = %q, want %q", evening.DailyDelta, "+0.7 kg")
	}
	if !evening.DailyGain {
		t.Error("DailyGain = false, want true")
	}
}

func TestBuildRowsMarksTodayByCalendarDay(t *testing.T) {
	entries := []db.Entry{
		// Same calendar day as now, but hours earlier — still "today".
		entry(3, at(t, "2026-09-20 08:12"), 81.0, ""),
		// Just before midnight the day before — not today, though only
		// hours away.
		entry(2, at(t, "2026-09-19 23:50"), 81.4, ""),
		entry(1, at(t, "2026-08-15 07:30"), 83.1, ""),
	}
	rows := BuildRows(entries, at(t, "2026-09-20 21:30"))

	if !rows[0].Today {
		t.Error("row recorded earlier on the current day: Today = false, want true")
	}
	if rows[1].Today {
		t.Error("row recorded the previous evening: Today = true, want false")
	}
	if rows[2].Today {
		t.Error("row from a month ago: Today = true, want false")
	}
}
