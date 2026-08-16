package main

import (
	"testing"

	"weight-tracker/internal/db"
)

func goal(id int64, weightKg float64, effectiveFrom string, t *testing.T) db.Goal {
	t.Helper()
	return db.Goal{ID: id, WeightG: db.KgToGrams(weightKg), EffectiveFrom: at(t, effectiveFrom+" 00:00")}
}

func TestBuildGoalRows(t *testing.T) {
	now := at(t, "2026-08-16 12:00")
	// Newest-first, as db.ListGoals returns them.
	goals := []db.Goal{
		goal(3, 76, "2026-12-01", t), // future
		goal(2, 78, "2026-08-01", t), // current
		goal(1, 80, "2026-01-01", t), // superseded
	}
	rows := buildGoalRows(goals, now)

	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].Current {
		t.Error("a future goal is marked current")
	}
	if !rows[1].Current {
		t.Error("the most recent already-effective goal is not marked current")
	}
	if rows[2].Current {
		t.Error("a superseded goal is marked current")
	}
	if rows[1].WeightKgStr != "78.0" {
		t.Errorf("WeightKgStr = %q, want %q", rows[1].WeightKgStr, "78.0")
	}
	if rows[1].EffectiveFromLabel != "Aug 1, 2026" {
		t.Errorf("EffectiveFromLabel = %q", rows[1].EffectiveFromLabel)
	}
	if rows[1].EffectiveFromDate != "2026-08-01" {
		t.Errorf("EffectiveFromDate = %q, want the edit form's input value", rows[1].EffectiveFromDate)
	}
}

func TestBuildGoalRowsMarksAtMostOneCurrent(t *testing.T) {
	now := at(t, "2026-08-16 12:00")
	goals := []db.Goal{
		goal(2, 78, "2026-08-01", t),
		goal(1, 80, "2026-07-01", t),
	}
	current := 0
	for _, row := range buildGoalRows(goals, now) {
		if row.Current {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d goals marked current, want exactly 1", current)
	}
}

func TestBuildGoalRowsWithOnlyFutureGoals(t *testing.T) {
	now := at(t, "2026-08-16 12:00")
	rows := buildGoalRows([]db.Goal{goal(1, 76, "2026-12-01", t)}, now)
	if rows[0].Current {
		t.Error("a goal that has not taken effect yet is marked current")
	}
}
