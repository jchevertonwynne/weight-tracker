package handlers

import (
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

// entriesWindow reads the period/range/from/until fields shared by the
// entries-filter form's own GET and by hx-include="#entries-filter" on the
// create/edit/delete actions, so a mutation's own response can be filtered
// to the same view the user is looking at instead of falling back to a
// default range.
//
// r.FormValue reads both the URL query and a form-encoded body, which
// covers every method these routes use: htmx puts included fields in the
// query string for GET and DELETE (its default methodsThatUseUrlParams),
// and in the body for POST/PUT — exactly the split Go's FormValue already
// understands.
func (s *Server) entriesWindow(r *http.Request) (periodParam string, window timerange.Window) {
	rangeParam := r.FormValue("range")
	if rangeParam == "" {
		rangeParam = "all"
	}
	window = timerange.Resolve(rangeParam, r.FormValue("from"), r.FormValue("until"), s.now())
	return r.FormValue("period"), window
}

// RenderEntriesList renders the history list filtered to whatever
// period/range/from/until the request carries, and asks the chart controls
// to refresh themselves too, since a changed entry can affect whatever
// range/series the user currently has selected. Used directly as the
// GET /entries handler, and called by the create/update/delete handlers
// below so their own response reflects the same filtered view rather than
// reverting to the default range.
func (s *Server) RenderEntriesList(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	periodParam, window := s.entriesWindow(r)
	w.Header().Set("HX-Trigger", "entries-changed")
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
	s.RenderEntriesList(w, r)
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
	s.RenderEntriesList(w, r)
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
	s.RenderEntriesList(w, r)
}
