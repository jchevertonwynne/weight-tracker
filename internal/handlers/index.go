package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jchevertonwynne/homelab-go/render"

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
	entries, err := db.ListEntries(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	goalList, err := db.ListGoals(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markerList, err := db.ListMarkers(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := s.now()
	// Each picker's range comes off the URL, so a reload — or a shared link —
	// lands on the same view rather than snapping back to the defaults. The
	// parameter naming ("overnight", "overnight_from", "overnight_until") is
	// the half of the contract the server owns; static/app.js writes the same
	// shape back when a range is applied.
	query := r.URL.Query()
	picker := func(param, def string) timerange.PickerConfig {
		return timerange.Picker(param, def, query.Get(param), query.Get(param+"_from"), query.Get(param+"_until"))
	}
	chartPicker := picker("chart", "30")
	historyPicker := picker("history", "all")
	overnightPicker := picker("overnight", "30")

	// Filtering through the pickers' own windows is what guarantees the
	// precomputed render agrees with what each picker claims to be showing.
	// The chart needs no equivalent: it is drawn client-side from /chart,
	// which chart-app.js requests using these same hidden inputs.
	overnightPairs := overnight.WindowedPairs(entries, overnightPicker.Window(now))
	// The period select has no URL parameter, so it is still at its "All"
	// default here — an empty periodParam, exactly what RenderEntriesList
	// would pass for it.
	historyRows := history.FilterRows(history.BuildRows(entries), "", historyPicker.Window(now))
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
		Rows:           historyRows,
		Goals:          goals.BuildRows(goalList, now),
		Markers:        markers.BuildRows(markerList),
		Summary:        summary.Build(entries, goalList, now),
		ChartRange:     chartPicker,
		HistoryRange:   historyPicker,
		OvernightRange: overnightPicker,
		Overnight:      overnight.BuildSummary(overnightPairs),
		Pairs:          overnightPairs,
	}
	// render.Named buffers before writing: writing straight to w commits a
	// 200 with the first byte, so a template that failed halfway used to
	// leave http.Error trying to set a 500 on an already-committed
	// response — Go logs that as "superfluous response.WriteHeader call"
	// and the client keeps the truncated 200.
	if err := render.Named(w, s.tmpl, "index", data); err != nil {
		slog.ErrorContext(r.Context(), "render index", "error", err)
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
	if _, err := w.Write([]byte("ok\n")); err != nil {
		slog.ErrorContext(r.Context(), "write healthz response", "error", err)
	}
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
	entries, err := db.ListEntries(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	goalList, err := db.ListGoals(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markerList, err := db.ListMarkers(r.Context(), s.db)
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
func (s *Server) HandleServiceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := s.staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := w.Write(b); err != nil {
		slog.ErrorContext(r.Context(), "write service worker", "error", err)
	}
}
