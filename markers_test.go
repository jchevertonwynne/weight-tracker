package main

import (
	"testing"

	"weight-tracker/internal/db"
)

func marker(id int64, date string, note string, t *testing.T) db.Marker {
	t.Helper()
	return db.Marker{ID: id, Date: at(t, date+" 00:00"), Note: note}
}

func TestBuildMarkerRows(t *testing.T) {
	rows := buildMarkerRows([]db.Marker{marker(1, "2026-08-10", "started cutting", t)})
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
