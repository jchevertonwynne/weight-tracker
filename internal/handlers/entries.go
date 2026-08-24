package handlers

import (
	"context"
	"net/http"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/history"
	"weight-tracker/internal/timerange"
	"weight-tracker/internal/weight"
)

// parseRecordedAt reads the split recorded_at_date/recorded_at_time fields.
func parseRecordedAt(r *http.Request) (time.Time, error) {
	return parseDateTimeFields(r, "recorded_at")
}

// RenderEntriesList re-renders the history list after a create/update/delete
// and asks the chart controls to refresh themselves too, since a changed
// entry can affect whatever range/series the user currently has selected.
func (s *Server) RenderEntriesList(ctx context.Context, w http.ResponseWriter) {
	entries, err := db.ListEntries(ctx, s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "entries-changed")
	data := struct{ Rows []history.Row }{Rows: history.BuildRows(entries)}
	if err := s.tmpl.ExecuteTemplate(w, "entries-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// HandleEntriesList renders the history list filtered by the entries-filter
// form — period, plus the exact same range/from/until triple the chart's
// own time-range-picker submits (it's the same shared component), so a
// preset like range=30 is resolved by timerange.Resolve the same way for
// both. Registered separately from the CRUD handlers below so a
// create/update/delete's own response can keep rendering the unfiltered
// list — the filter form re-applies itself afterward via
// hx-trigger="... entries-changed from:body", so the visible list ends up
// filtered either way, just via one extra round-trip.
func (s *Server) HandleEntriesList(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	periodParam := r.URL.Query().Get("period")
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "all"
	}
	window := timerange.Resolve(rangeParam, r.URL.Query().Get("from"), r.URL.Query().Get("until"), s.now())
	data := struct{ Rows []history.Row }{Rows: history.FilterRows(history.BuildRows(entries), periodParam, window)}
	if err := s.tmpl.ExecuteTemplate(w, "entries-list", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) HandleCreate(w http.ResponseWriter, r *http.Request) {
	weightG, err := parseWeightG(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recordedAt, err := parseRecordedAt(r)
	if err != nil {
		http.Error(w, "invalid recorded_at", http.StatusBadRequest)
		return
	}
	periodOverride := r.FormValue("period_override")
	if !weight.ValidPeriodOverride(periodOverride) {
		http.Error(w, "invalid period_override", http.StatusBadRequest)
		return
	}
	if _, err := db.CreateEntry(r.Context(), s.db, recordedAt, weightG, periodOverride, s.now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.RenderEntriesList(r.Context(), w)
}

func (s *Server) HandleEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	entry, err := db.GetEntry(r.Context(), s.db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rows := history.BuildRows([]db.Entry{entry})
	if err := s.tmpl.ExecuteTemplate(w, "row-edit", rows[0]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) HandleCancelEdit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	entries, err := db.ListEntries(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range history.BuildRows(entries) {
		if row.ID == id {
			if err := s.tmpl.ExecuteTemplate(w, "row", row); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) HandleUpdate(w http.ResponseWriter, r *http.Request) {
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
	recordedAt, err := parseRecordedAt(r)
	if err != nil {
		http.Error(w, "invalid recorded_at", http.StatusBadRequest)
		return
	}
	periodOverride := r.FormValue("period_override")
	if !weight.ValidPeriodOverride(periodOverride) {
		http.Error(w, "invalid period_override", http.StatusBadRequest)
		return
	}
	if err := db.UpdateEntry(r.Context(), s.db, id, recordedAt, weightG, periodOverride); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.RenderEntriesList(r.Context(), w)
}

func (s *Server) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDPath(r)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteEntry(r.Context(), s.db, id); err != nil {
		writeDeleteError(w, err)
		return
	}
	s.RenderEntriesList(r.Context(), w)
}
