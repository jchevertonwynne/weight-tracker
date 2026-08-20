// Package goals holds the display-ready Row form of a db.Goal, the current
// in-effect goal logic, and the time-bounded Segment breakdown used to draw
// the chart's goal reference line.
package goals

import (
	"sort"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/weight"
)

// Row is the display-ready form of a db.Goal for the goal-history list.
type Row struct {
	ID                 int64
	WeightKgRaw        string
	WeightKgStr        string
	EffectiveFromLabel string
	EffectiveFromDate  string // for the edit form's date input
	Current            bool
}

// Current returns the goal in effect at now. goals is newest-first (as
// db.ListGoals returns it), so the first entry whose EffectiveFrom is at or
// before now is the current one — anything later hasn't taken effect yet.
func Current(goals []db.Goal, now time.Time) (db.Goal, bool) {
	for _, g := range goals {
		if !g.EffectiveFrom.After(now) {
			return g, true
		}
	}
	return db.Goal{}, false
}

// BuildRows assumes goals is newest-first (as returned by db.ListGoals).
func BuildRows(goals []db.Goal, now time.Time) []Row {
	current, hasCurrent := Current(goals, now)
	rows := make([]Row, len(goals))
	for i, g := range goals {
		rows[i] = Row{
			ID:                 g.ID,
			WeightKgRaw:        weight.FormatKgInput(g.WeightG),
			WeightKgStr:        weight.FormatKg(g.WeightG),
			EffectiveFromLabel: g.EffectiveFrom.Format("Jan 2, 2006"),
			EffectiveFromDate:  g.EffectiveFrom.Format("2006-01-02"),
			Current:            hasCurrent && g.ID == current.ID,
		}
	}
	return rows
}

// Segment is one time-bounded goal validity period: [From, Until). A zero
// Until means open-ended (the most recent goal).
type Segment struct {
	WeightG int64
	From    time.Time
	Until   time.Time
}

// BuildSegments turns goals (any order) into non-overlapping segments: each
// goal is valid from its own EffectiveFrom until the next goal's
// EffectiveFrom (exclusive).
func BuildSegments(goals []db.Goal) []Segment {
	if len(goals) == 0 {
		return nil
	}
	sorted := make([]db.Goal, len(goals))
	copy(sorted, goals)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].EffectiveFrom.Before(sorted[j].EffectiveFrom)
	})

	segs := make([]Segment, len(sorted))
	for i, g := range sorted {
		seg := Segment{WeightG: g.WeightG, From: g.EffectiveFrom}
		if i+1 < len(sorted) {
			seg.Until = sorted[i+1].EffectiveFrom
		}
		segs[i] = seg
	}
	return segs
}

// ClipSegments returns the portion of each segment that overlaps [from,
// until]; open-ended segments are closed off at until. Segments with no
// overlap are dropped entirely.
func ClipSegments(segs []Segment, from, until time.Time) []Segment {
	var out []Segment
	for _, s := range segs {
		segFrom, segUntil := s.From, s.Until
		if segUntil.IsZero() || segUntil.After(until) {
			segUntil = until
		}
		if segFrom.Before(from) {
			segFrom = from
		}
		if !segFrom.Before(segUntil) {
			continue
		}
		out = append(out, Segment{WeightG: s.WeightG, From: segFrom, Until: segUntil})
	}
	return out
}
