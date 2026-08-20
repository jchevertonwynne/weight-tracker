package main

import (
	"encoding/json"
	"net/http"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/overnight"
	"weight-tracker/internal/timerange"
)

// handleOvernightTab renders the Overnight tab's stats/calculator/pairs-table
// fragment, filtered by the same range/from/until triple the chart and
// History filter submit via the shared time-range-picker.
func (s *server) handleOvernightTab(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "30"
	}
	window := timerange.Resolve(rangeParam, r.URL.Query().Get("from"), r.URL.Query().Get("until"), time.Now())
	pairs := overnight.WindowedPairs(entries, window)

	// Field names (Overnight/Pairs) match handleIndex's full data struct, so
	// the "overnight-content" template works from either — the same pattern
	// entries-list already uses (Rows) across handleIndex and
	// renderEntriesList's narrower struct.
	data := struct {
		Overnight overnight.Summary
		Pairs     []overnight.Pair
	}{
		Overnight: overnight.BuildSummary(pairs),
		Pairs:     pairs,
	}
	if err := tmpl.ExecuteTemplate(w, "overnight-content", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleOvernightWindows returns the "Range by timescale" chart data as
// JSON, mirroring handleChart's split: no server-side pixel math, the
// client-side Chart.js instance handles that.
func (s *server) handleOvernightWindows(w http.ResponseWriter, r *http.Request) {
	entries, err := db.ListEntries(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	goals, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(overnight.BuildWindowChart(entries, goals, time.Now())); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
