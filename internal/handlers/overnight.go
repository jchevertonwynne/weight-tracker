package handlers

import (
	"encoding/json"
	"net/http"

	"weight-tracker/internal/db"
	"weight-tracker/internal/overnight"
	"weight-tracker/internal/timerange"
)

// HandleOvernightTab renders the Overnight tab's stats/calculator/pairs-table
// fragment, filtered by the same range/from/until triple the chart and
// History filter submit via the shared time-range-picker.
func (s *Server) HandleOvernightTab(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "30"
	}
	window := timerange.Resolve(rangeParam, r.URL.Query().Get("from"), r.URL.Query().Get("until"), s.now())
	pairs := overnight.WindowedPairs(entries, window)

	// Field names (Overnight/Pairs) match HandleIndex's full data struct, so
	// the "overnight-content" template works from either — the same pattern
	// entries-list already uses (Rows) across HandleIndex and
	// RenderEntriesList's narrower struct.
	data := struct {
		Overnight overnight.Summary
		Pairs     []overnight.Pair
	}{
		Overnight: overnight.BuildSummary(pairs),
		Pairs:     pairs,
	}
	if err := s.tmpl.ExecuteTemplate(w, "overnight-content", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// HandleOvernightWindows returns the "Range by timescale" chart data as
// JSON, mirroring HandleChart's split: no server-side pixel math, the
// client-side Chart.js instance handles that.
func (s *Server) HandleOvernightWindows(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	goalList, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(overnight.BuildWindowChart(entries, goalList, s.now())); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
