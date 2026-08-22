package markers

import (
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/testsupport"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func marker(id int64, date string, note string, t *testing.T) db.Marker {
	return testsupport.Marker(id, date, note, t)
}

func TestBuildRows(t *testing.T) {
	rows := BuildRows([]db.Marker{marker(1, "2026-08-10", "started cutting", t)})
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].DateLabel != "Aug 10, 2026" {
		t.Errorf("DateLabel = %q", rows[0].DateLabel)
	}
	if rows[0].DateInput != "2026-08-10" {
		t.Errorf("DateInput = %q, want the edit form's input value", rows[0].DateInput)
	}
	if rows[0].Note != "started cutting" {
		t.Errorf("Note = %q", rows[0].Note)
	}
}

func TestVisible(t *testing.T) {
	markers := []db.Marker{
		marker(1, "2026-08-01", "before", t),
		marker(2, "2026-08-10", "inside", t),
		marker(3, "2026-08-20", "after", t),
	}

	t.Run("keeps only markers inside the range", func(t *testing.T) {
		got := Visible(markers, at(t, "2026-08-05 07:00"), at(t, "2026-08-15 21:00"))
		if len(got) != 1 {
			t.Fatalf("got %d markers, want 1", len(got))
		}
		if got[0].Note != "inside" {
			t.Errorf("kept %q, want the marker inside the range", got[0].Note)
		}
		if got[0].ID != 2 {
			t.Errorf("ID = %d, want 2 — the client colors markers by stable ID", got[0].ID)
		}
		if got[0].Date != "Aug 10, 2026" {
			t.Errorf("Date = %q, want %q", got[0].Date, "Aug 10, 2026")
		}
	})

	// Markers are date-only (midnight), so a marker on the same day as the
	// first visible entry sits earlier in that day than the entry's own
	// timestamp — comparing exact instants would wrongly exclude it.
	t.Run("includes a marker sharing the first entry's day", func(t *testing.T) {
		got := Visible(markers, at(t, "2026-08-10 07:30"), at(t, "2026-08-15 21:00"))
		if len(got) != 1 {
			t.Fatalf("got %d markers, want the same-day marker kept", len(got))
		}
	})

	t.Run("includes a marker sharing the last entry's day", func(t *testing.T) {
		got := Visible(markers, at(t, "2026-08-05 07:00"), at(t, "2026-08-10 07:30"))
		if len(got) != 1 {
			t.Fatalf("got %d markers, want the same-day marker kept", len(got))
		}
	})

	t.Run("no markers in range yields nothing", func(t *testing.T) {
		got := Visible(markers, at(t, "2026-09-01 07:00"), at(t, "2026-09-30 21:00"))
		if len(got) != 0 {
			t.Errorf("got %+v, want nothing", got)
		}
	})

	t.Run("no markers at all", func(t *testing.T) {
		got := Visible(nil, at(t, "2026-08-05 07:00"), at(t, "2026-08-15 21:00"))
		if len(got) != 0 {
			t.Errorf("got %+v, want nothing", got)
		}
	})
}
