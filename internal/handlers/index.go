package handlers

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"

	"weight-tracker/internal/chart"
	"weight-tracker/internal/db"
	"weight-tracker/internal/goals"
	"weight-tracker/internal/history"
	"weight-tracker/internal/markers"
	"weight-tracker/internal/overnight"
	"weight-tracker/internal/summary"
	"weight-tracker/internal/timerange"
)

func (s *Server) HandleIndex(w http.ResponseWriter, r *http.Request) {
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
	markerList, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := s.now()
	// Matches OvernightRange's DefaultRange below, so the precomputed initial
	// render agrees with what the picker claims to be showing.
	overnightWindow := timerange.Resolve("30", "", "", now)
	overnightPairs := overnight.WindowedPairs(entries, overnightWindow)
	data := struct {
		NowDate        string
		NowTime        string
		Rows           []history.Row
		Goals          []goals.Row
		Markers        []markers.Row
		Summary        summary.WeeklySummary
		ChartRange     timerange.PickerConfig
		HistoryRange   timerange.PickerConfig
		OvernightRange timerange.PickerConfig
		Overnight      overnight.Summary
		Pairs          []overnight.Pair
	}{
		NowDate:        now.Format("2006-01-02"),
		NowTime:        now.Format("15:04"),
		Rows:           history.BuildRows(entries),
		Goals:          goals.BuildRows(goalList, now),
		Markers:        markers.BuildRows(markerList),
		Summary:        summary.Build(entries, now),
		ChartRange:     timerange.PickerConfig{DefaultRange: "30", DefaultLabel: "Last 30 days"},
		HistoryRange:   timerange.PickerConfig{DefaultRange: "all", DefaultLabel: "All time"},
		OvernightRange: timerange.PickerConfig{DefaultRange: "30", DefaultLabel: "Last 30 days"},
		Overnight:      overnight.BuildSummary(overnightPairs),
		Pairs:          overnightPairs,
	}
	// Rendered into a buffer rather than straight to w. Writing to the
	// ResponseWriter commits a 200 with the first byte, so a template that
	// failed halfway left http.Error trying to set a 500 on an
	// already-committed response — Go logs that as "superfluous
	// response.WriteHeader call" and the client keeps the truncated 200.
	// Buffering means a genuine template error becomes a clean 500 with no
	// partial page, and the only thing that can fail afterwards is the write
	// itself, which nothing can be done about but log.
	//
	// The page is a few tens of kilobytes, so holding one in memory is
	// cheaper than the alternative being wrong.
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "index", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("write index response: %v", err)
	}
}

// HandleHealthz is a liveness endpoint for the Kubernetes probes. It exists so
// they do not have to hit "/", which lists every entry, goal and marker and
// rebuilds the summary on each call — several times a minute, forever, on a
// Raspberry Pi. It deliberately does not touch the database: this answers
// "is the process serving?", and a failing database should surface as a 500
// on a real request rather than a restart loop.
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("ok\n"))
}

// HandleChart returns chart data as JSON for the client-side Chart.js
// instance to render — no server-side pixel math or HTML fragment.
func (s *Server) HandleChart(w http.ResponseWriter, r *http.Request) {
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "30"
	}
	seriesParam := r.URL.Query().Get("series")
	if seriesParam == "" {
		seriesParam = "all"
	}
	fromParam := r.URL.Query().Get("from")
	untilParam := r.URL.Query().Get("until")
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
	markerList, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := chart.Build(entries, goalList, markerList, rangeParam, seriesParam, fromParam, untilParam, s.now())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// HandleServiceWorker serves the service worker from a top-level path so its
// default scope is "/" (the whole app), not "/static/" — a service worker's
// scope defaults to the directory of its own URL.
func (s *Server) HandleServiceWorker(w http.ResponseWriter, _ *http.Request) {
	b, err := s.staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
