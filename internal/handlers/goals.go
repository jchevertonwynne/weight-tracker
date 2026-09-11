package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"weight-tracker/internal/db"
	"weight-tracker/internal/goals"

	"github.com/jchevertonwynne/homelab-go/render"
)

// RenderGoalsList re-renders the goals-list card and fires goals-changed so
// the chart controls (which also affect the plotted goal reference lines)
// refresh themselves too.
func (s *Server) RenderGoalsList(ctx context.Context, w http.ResponseWriter) {
	goalList, err := db.ListGoals(ctx, s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "goals-changed")
	data := struct{ Goals []goals.Row }{Goals: goals.BuildRows(goalList, s.now())}
	if err := render.Named(w, s.tmpl, "goals-list", data); err != nil {
		slog.ErrorContext(ctx, "render goals-list", "error", err)
	}
}

func (s *Server) HandleGoalCreate(w http.ResponseWriter, r *http.Request) {
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
	if _, err := db.CreateGoal(r.Context(), s.db, weightG, effectiveFrom, s.now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.RenderGoalsList(r.Context(), w)
}

func (s *Server) HandleGoalEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	goal, err := db.GetGoal(r.Context(), s.db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rows := goals.BuildRows([]db.Goal{goal}, s.now())
	if err := render.Named(w, s.tmpl, "goal-row-edit", rows[0]); err != nil {
		slog.ErrorContext(r.Context(), "render goal-row-edit", "error", err)
	}
}

func (s *Server) HandleGoalCancelEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	goalList, err := db.ListGoals(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range goals.BuildRows(goalList, s.now()) {
		if row.ID == id {
			if err := render.Named(w, s.tmpl, "goal-row", row); err != nil {
				slog.ErrorContext(r.Context(), "render goal-row", "error", err)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) HandleGoalUpdate(w http.ResponseWriter, r *http.Request) {
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
	if err := db.UpdateGoal(r.Context(), s.db, id, weightG, effectiveFrom); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.RenderGoalsList(r.Context(), w)
}

func (s *Server) HandleGoalDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteGoal(r.Context(), s.db, id); err != nil {
		writeDeleteError(w, err)
		return
	}
	s.RenderGoalsList(r.Context(), w)
}
