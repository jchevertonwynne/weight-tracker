package goals

import (
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/testsupport"
)

func at(t *testing.T, s string) time.Time { return testsupport.At(t, s) }

func goal(id int64, weightKg float64, effectiveFrom string, t *testing.T) db.Goal {
	return testsupport.Goal(id, weightKg, effectiveFrom, t)
}

func TestBuildRows(t *testing.T) {
	now := at(t, "2026-08-16 12:00")
	// Newest-first, as db.ListGoals returns them.
	goals := []db.Goal{
		goal(3, 76, "2026-12-01", t), // future
		goal(2, 78, "2026-08-01", t), // current
		goal(1, 80, "2026-01-01", t), // superseded
	}
	rows := BuildRows(goals, now)

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

func TestBuildRowsMarksAtMostOneCurrent(t *testing.T) {
	now := at(t, "2026-08-16 12:00")
	goals := []db.Goal{
		goal(2, 78, "2026-08-01", t),
		goal(1, 80, "2026-07-01", t),
	}
	current := 0
	for _, row := range BuildRows(goals, now) {
		if row.Current {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d goals marked current, want exactly 1", current)
	}
}

func TestBuildRowsWithOnlyFutureGoals(t *testing.T) {
	now := at(t, "2026-08-16 12:00")
	rows := BuildRows([]db.Goal{goal(1, 76, "2026-12-01", t)}, now)
	if rows[0].Current {
		t.Error("a goal that has not taken effect yet is marked current")
	}
}

func TestBuildSegments(t *testing.T) {
	t.Run("each goal runs until the next one starts", func(t *testing.T) {
		// Deliberately unsorted input.
		goals := []db.Goal{
			goal(2, 78, "2026-06-01", t),
			goal(1, 80, "2026-01-01", t),
			goal(3, 76, "2026-09-01", t),
		}
		segs := BuildSegments(goals)
		if len(segs) != 3 {
			t.Fatalf("got %d segments, want 3", len(segs))
		}
		if segs[0].WeightG != 80000 {
			t.Errorf("first segment = %v g, want the earliest goal (80000)", segs[0].WeightG)
		}
		if !segs[0].Until.Equal(segs[1].From) {
			t.Error("segments are not contiguous: segment 0 does not end where segment 1 begins")
		}
		if !segs[2].Until.IsZero() {
			t.Errorf("last segment ends at %v, want open-ended", segs[2].Until)
		}
	})

	t.Run("no goals yields no segments", func(t *testing.T) {
		if segs := BuildSegments(nil); segs != nil {
			t.Errorf("got %v, want nil", segs)
		}
	})

	t.Run("a single goal is one open-ended segment", func(t *testing.T) {
		segs := BuildSegments([]db.Goal{goal(1, 78, "2026-01-01", t)})
		if len(segs) != 1 || !segs[0].Until.IsZero() {
			t.Errorf("got %+v, want one open-ended segment", segs)
		}
	})
}

func TestClipSegments(t *testing.T) {
	segs := BuildSegments([]db.Goal{
		goal(1, 80, "2026-01-01", t),
		goal(2, 78, "2026-06-01", t),
	})

	t.Run("closes off the open-ended segment at the range end", func(t *testing.T) {
		from, until := at(t, "2026-05-01 00:00"), at(t, "2026-08-16 00:00")
		got := ClipSegments(segs, from, until)
		if len(got) != 2 {
			t.Fatalf("got %d segments, want 2", len(got))
		}
		if !got[0].From.Equal(from) {
			t.Errorf("first segment starts at %v, want clamped to %v", got[0].From, from)
		}
		if !got[1].Until.Equal(until) {
			t.Errorf("open-ended segment ends at %v, want clamped to %v", got[1].Until, until)
		}
	})

	t.Run("drops segments with no overlap", func(t *testing.T) {
		from, until := at(t, "2026-07-01 00:00"), at(t, "2026-08-16 00:00")
		got := ClipSegments(segs, from, until)
		if len(got) != 1 {
			t.Fatalf("got %d segments, want only the 78 kg one", len(got))
		}
		if got[0].WeightG != 78000 {
			t.Errorf("kept the %v g segment, want 78000", got[0].WeightG)
		}
	})

	t.Run("a range entirely before every goal keeps nothing", func(t *testing.T) {
		got := ClipSegments(segs, at(t, "2025-01-01 00:00"), at(t, "2025-06-01 00:00"))
		if len(got) != 0 {
			t.Errorf("got %+v, want nothing", got)
		}
	})

	t.Run("a zero-width overlap is dropped rather than emitted", func(t *testing.T) {
		// The range ends exactly where the first segment starts.
		got := ClipSegments(segs, at(t, "2025-06-01 00:00"), at(t, "2026-01-01 00:00"))
		for _, s := range got {
			if !s.From.Before(s.Until) {
				t.Errorf("emitted a zero-width segment %+v", s)
			}
		}
	})
}
