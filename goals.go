package main

import (
	"net/http"
	"sort"
	"time"

	"weight-tracker/internal/db"
)

// GoalRow is the display-ready form of a db.Goal for the goal-history list.
type GoalRow struct {
	ID                 int64
	WeightKgRaw        string
	WeightKgStr        string
	EffectiveFromLabel string
	EffectiveFromDate  string // for the edit form's date input
	Current            bool
}

// buildGoalRows assumes goals is newest-first (as returned by db.ListGoals).
// Current is set on the first goal (scanning newest-first) whose
// EffectiveFrom is at or before now.
func buildGoalRows(goals []db.Goal, now time.Time) []GoalRow {
	rows := make([]GoalRow, len(goals))
	foundCurrent := false
	for i, g := range goals {
		row := GoalRow{
			ID:                 g.ID,
			WeightKgRaw:        formatKgInput(g.WeightG),
			WeightKgStr:        formatKg(g.WeightG),
			EffectiveFromLabel: g.EffectiveFrom.Format("Jan 2, 2006"),
			EffectiveFromDate:  g.EffectiveFrom.Format("2006-01-02"),
		}
		if !foundCurrent && !g.EffectiveFrom.After(now) {
			row.Current = true
			foundCurrent = true
		}
		rows[i] = row
	}
	return rows
}

// GoalSegment is one time-bounded goal validity period: [From, Until).
// A zero Until means open-ended (the most recent goal).
type GoalSegment struct {
	WeightG int64
	From    time.Time
	Until   time.Time
}

// buildGoalSegments turns goals (any order) into non-overlapping segments:
// each goal is valid from its own EffectiveFrom until the next goal's
// EffectiveFrom (exclusive).
func buildGoalSegments(goals []db.Goal) []GoalSegment {
	if len(goals) == 0 {
		return nil
	}
	sorted := make([]db.Goal, len(goals))
	copy(sorted, goals)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].EffectiveFrom.Before(sorted[j].EffectiveFrom)
	})

	segs := make([]GoalSegment, len(sorted))
	for i, g := range sorted {
		seg := GoalSegment{WeightG: g.WeightG, From: g.EffectiveFrom}
		if i+1 < len(sorted) {
			seg.Until = sorted[i+1].EffectiveFrom
		}
		segs[i] = seg
	}
	return segs
}

// clipGoalSegments returns the portion of each segment that overlaps
// [from, until]; open-ended segments are closed off at until. Segments with
// no overlap are dropped entirely.
func clipGoalSegments(segs []GoalSegment, from, until time.Time) []GoalSegment {
	var out []GoalSegment
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
		out = append(out, GoalSegment{WeightG: s.WeightG, From: segFrom, Until: segUntil})
	}
	return out
}

// renderGoalsList re-renders the goals-list card and fires goals-changed so
// the chart controls (which also affect the plotted goal reference lines)
// refresh themselves too.
func (s *server) renderGoalsList(w http.ResponseWriter) {
	goals, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "goals-changed")
	data := struct{ Goals []GoalRow }{Goals: buildGoalRows(goals, time.Now())}
	if err := tmpl.ExecuteTemplate(w, "goals-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleGoalCreate(w http.ResponseWriter, r *http.Request) {
	weightG, err := parseWeightG(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	effectiveFrom, err := parseDateField(r, "effective_from")
	if err != nil {
		http.Error(w, "invalid effective_from", http.StatusBadRequest)
		return
	}
	if _, err := db.CreateGoal(s.db, weightG, effectiveFrom, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderGoalsList(w)
}

func (s *server) handleGoalEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	goal, err := db.GetGoal(s.db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rows := buildGoalRows([]db.Goal{goal}, time.Now())
	if err := tmpl.ExecuteTemplate(w, "goal-row-edit", rows[0]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleGoalCancelEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	goals, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range buildGoalRows(goals, time.Now()) {
		if row.ID == id {
			if err := tmpl.ExecuteTemplate(w, "goal-row", row); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *server) handleGoalUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	weightG, err := parseWeightG(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	effectiveFrom, err := parseDateField(r, "effective_from")
	if err != nil {
		http.Error(w, "invalid effective_from", http.StatusBadRequest)
		return
	}
	if err := db.UpdateGoal(s.db, id, weightG, effectiveFrom); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderGoalsList(w)
}

func (s *server) handleGoalDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteGoal(s.db, id); err != nil {
		writeDeleteError(w, err)
		return
	}
	s.renderGoalsList(w)
}
