package main

import (
	"net/http"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/goals"
)

// renderGoalsList re-renders the goals-list card and fires goals-changed so
// the chart controls (which also affect the plotted goal reference lines)
// refresh themselves too.
func (s *server) renderGoalsList(w http.ResponseWriter) {
	goalList, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "goals-changed")
	data := struct{ Goals []goals.Row }{Goals: goals.BuildRows(goalList, time.Now())}
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
	rows := goals.BuildRows([]db.Goal{goal}, time.Now())
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
	goalList, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range goals.BuildRows(goalList, time.Now()) {
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
