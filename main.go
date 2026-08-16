package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"weight-tracker/internal/db"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.html"))

type server struct {
	db *sql.DB
	// grafanaEnabled gates the Graphs tab. With no Grafana to proxy to,
	// showing a tab whose only content is a broken iframe helps nobody.
	grafanaEnabled bool
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "weight-tracker.db", "path to sqlite database file")
	grafanaURL := flag.String("grafana", "http://127.0.0.1:3000",
		"base URL of the Grafana to proxy under /grafana/ (empty to disable the Graphs tab)")
	flag.Parse()

	sqlDB, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()

	s := &server{db: sqlDB}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /chart", s.handleChart)
	mux.HandleFunc("GET /summary", s.handleSummary)
	mux.HandleFunc("GET /sw.js", s.handleServiceWorker)
	mux.HandleFunc("POST /entries", s.handleCreate)
	mux.HandleFunc("GET /entries/{id}", s.handleCancelEdit)
	mux.HandleFunc("GET /entries/{id}/edit", s.handleEdit)
	mux.HandleFunc("PUT /entries/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE /entries/{id}", s.handleDelete)
	mux.HandleFunc("POST /goals", s.handleGoalCreate)
	mux.HandleFunc("GET /goals/{id}", s.handleGoalCancelEdit)
	mux.HandleFunc("GET /goals/{id}/edit", s.handleGoalEdit)
	mux.HandleFunc("PUT /goals/{id}", s.handleGoalUpdate)
	mux.HandleFunc("DELETE /goals/{id}", s.handleGoalDelete)
	mux.HandleFunc("POST /markers", s.handleMarkerCreate)
	mux.HandleFunc("GET /markers/{id}", s.handleMarkerCancelEdit)
	mux.HandleFunc("GET /markers/{id}/edit", s.handleMarkerEdit)
	mux.HandleFunc("PUT /markers/{id}", s.handleMarkerUpdate)
	mux.HandleFunc("DELETE /markers/{id}", s.handleMarkerDelete)
	mux.HandleFunc("GET /export.csv", s.handleExport)
	mux.HandleFunc("GET /backup.db", s.handleBackup)
	mux.HandleFunc("GET /import", s.handleImportForm)
	mux.HandleFunc("POST /import", s.handleImport)
	mux.HandleFunc("POST /settings/delete-all", s.handleDeleteAll)

	if *grafanaURL != "" {
		proxy, err := newGrafanaProxy(*grafanaURL)
		if err != nil {
			log.Fatalf("grafana proxy: %v", err)
		}
		mux.Handle("/grafana/", proxy)
		s.grafanaEnabled = true
		log.Printf("proxying /grafana/ to %s", *grafanaURL)
	}

	journalMode, err := db.JournalMode(sqlDB)
	if err != nil {
		log.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		// Not fatal — the rollback journal is still correct — but worth
		// saying out loud, since it usually means the database lives on a
		// filesystem that cannot do WAL.
		log.Printf("warning: journal mode is %q, not WAL", journalMode)
	}

	log.Printf("weight-tracker listening on %s (db: %s, journal: %s)", *addr, *dbPath, journalMode)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
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
	markers, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	data := struct {
		NowDate        string
		NowTime        string
		Rows           []Row
		Goals          []GoalRow
		Markers        []MarkerRow
		Summary        WeeklySummary
		GrafanaEnabled bool
		// EarliestMs is the oldest weigh-in as Unix milliseconds, so the
		// Graphs tab's "All time" range can ask Grafana for exactly the
		// span that holds data instead of guessing at some wide window and
		// leaving the series squashed against one edge.
		EarliestMs int64
	}{
		NowDate:        now.Format("2006-01-02"),
		NowTime:        now.Format("15:04"),
		Rows:           buildRows(entries),
		Goals:          buildGoalRows(goals, now),
		Markers:        buildMarkerRows(markers),
		Summary:        buildWeeklySummary(entries, now),
		GrafanaEnabled: s.grafanaEnabled,
		EarliestMs:     earliestMs(entries, now),
	}
	if err := tmpl.ExecuteTemplate(w, "index", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleChart returns chart data as JSON for the client-side Chart.js
// instance to render — no server-side pixel math or HTML fragment.
func (s *server) handleChart(w http.ResponseWriter, r *http.Request) {
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
	goals, err := db.ListGoals(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markers, err := db.ListMarkers(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := buildChartData(entries, goals, markers, rangeParam, seriesParam, fromParam, untilParam, time.Now())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleServiceWorker serves the service worker from a top-level path so its
// default scope is "/" (the whole app), not "/static/" — a service worker's
// scope defaults to the directory of its own URL.
func (s *server) handleServiceWorker(w http.ResponseWriter, _ *http.Request) {
	b, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// parseIDPath reads the {id} path value shared by every entity's
// edit/cancel-edit/update/delete routes.
func parseIDPath(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// earliestMs returns the oldest weigh-in as Unix milliseconds, falling back
// to now when there are no entries at all — an empty chart over a zero-width
// range is no worse than an empty chart over any other.
func earliestMs(entries []db.Entry, now time.Time) int64 {
	earliest := now
	for _, e := range entries {
		if e.RecordedAt.Before(earliest) {
			earliest = e.RecordedAt
		}
	}
	return earliest.UnixMilli()
}

// writeDeleteError maps a failed Delete* onto a status code: a row that
// isn't there is the client's problem (404), anything else is ours (500).
// Without this, deleting an already-deleted id reported success.
func writeDeleteError(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// maxWeightKg is an upper sanity bound on any weight the app will store,
// well clear of the heaviest recorded human but low enough to catch a
// mistyped or unit-confused value.
const maxWeightKg = 1000

// parseWeightG reads and validates the weight_kg form field shared by the
// entry and goal forms, returning the whole grams the database stores. The
// form still speaks kilograms because that is what the user reads off the
// scale; grams are an internal representation (see db.KgToGrams).
//
// The inputs carry min/step attributes, but those are client-side only —
// anything posting directly to the API could otherwise store a negative,
// zero, or non-finite weight, which then flows into the chart, the deltas,
// and the CSV export. importer.Parse applies the same positivity rule to
// bulk-imported rows.
func parseWeightG(r *http.Request) (int64, error) {
	weightKg, err := strconv.ParseFloat(r.FormValue("weight_kg"), 64)
	if err != nil {
		return 0, fmt.Errorf("weight_kg is not a number: %w", err)
	}
	// ParseFloat happily accepts "NaN" and "Inf"; neither survives being
	// averaged into a trend line, and neither converts to a sane gram count.
	if math.IsNaN(weightKg) || math.IsInf(weightKg, 0) {
		return 0, fmt.Errorf("weight_kg must be a finite number")
	}
	if weightKg <= 0 || weightKg > maxWeightKg {
		return 0, fmt.Errorf("weight_kg must be between 0 and %d, got %g", maxWeightKg, weightKg)
	}
	// Rounding to the nearest gram can only reach zero from a value below
	// half a gram, which the range check above has already let through.
	grams := db.KgToGrams(weightKg)
	if grams <= 0 {
		return 0, fmt.Errorf("weight_kg %g rounds to zero grams", weightKg)
	}
	return grams, nil
}

// dateTimeLayout matches the concatenation of an <input type="date"> value
// ("2006-01-02") and an <input type="time"> value ("15:04"), joined with
// "T" — neither carries a timezone of its own, so the combined value is
// parsed as local wall-clock time. Using separate date/time inputs rather
// than a single <input type="datetime-local"> avoids a clunky two-screen
// (calendar, then clock) native picker flow on some mobile browsers.
const dateTimeLayout = "2006-01-02T15:04"

func parseDateTimeFields(r *http.Request, prefix string) (time.Time, error) {
	combined := r.FormValue(prefix+"_date") + "T" + r.FormValue(prefix+"_time")
	return time.ParseInLocation(dateTimeLayout, combined, time.Local)
}

// dateOnlyLayout matches a lone <input type="date"> value, for fields (like
// a marker's date) that have no meaningful time-of-day component.
const dateOnlyLayout = "2006-01-02"

func parseDateField(r *http.Request, field string) (time.Time, error) {
	return time.ParseInLocation(dateOnlyLayout, r.FormValue(field), time.Local)
}
