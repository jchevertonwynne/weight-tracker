package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"html/template"
	"log"
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
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "weight-tracker.db", "path to sqlite database file")
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
	mux.HandleFunc("GET /import", s.handleImportForm)
	mux.HandleFunc("POST /import", s.handleImport)
	mux.HandleFunc("POST /settings/delete-all", s.handleDeleteAll)

	log.Printf("weight-tracker listening on %s (db: %s)", *addr, *dbPath)
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
		NowDate string
		NowTime string
		Rows    []Row
		Goals   []GoalRow
		Markers []MarkerRow
		Summary WeeklySummary
	}{
		NowDate: now.Format("2006-01-02"),
		NowTime: now.Format("15:04"),
		Rows:    buildRows(entries),
		Goals:   buildGoalRows(goals, now),
		Markers: buildMarkerRows(markers),
		Summary: buildWeeklySummary(entries, now),
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
	data := buildChartData(entries, goals, markers, rangeParam, seriesParam, time.Now())
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
