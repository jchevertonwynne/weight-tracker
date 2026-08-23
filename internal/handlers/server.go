// Package handlers holds every HTTP handler for the weight-tracker app,
// plus the Server type that carries their shared dependencies (the
// database, parsed templates, embedded static assets, and the clock).
// main.go's only job is to gather those dependencies and call New/
// RegisterRoutes — the handlers themselves, and the route table, live
// here.
package handlers

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"weight-tracker/internal/db"
)

// Server carries every handler's dependencies. now is injected rather than
// calling time.Now() directly, so tests can pin the clock instead of
// depending on the real wall clock; production wiring passes time.Now
// itself (see New's doc comment).
type Server struct {
	db       *sql.DB
	tmpl     *template.Template
	staticFS embed.FS
	now      func() time.Time
}

// New builds a Server. templatesFS/staticFS are the embed.FS values
// declared in main.go — embed paths can't reach outside the directory
// that declares them, and templates/ and static/ live at the repo root,
// so main.go must own the //go:embed directives and pass the results in
// here rather than this package embedding them itself. Pass time.Now for
// now in production; tests can pass a fixed closure instead.
func New(sqlDB *sql.DB, templatesFS, staticFS embed.FS, now func() time.Time) *Server {
	return &Server{
		db:       sqlDB,
		tmpl:     template.Must(template.ParseFS(templatesFS, "templates/*.html")),
		staticFS: staticFS,
		now:      now,
	}
}

// RegisterRoutes wires every route onto mux. Kept as one method rather
// than scattered registration calls so the full route table stays visible
// in one place, and so main.go doesn't need to know it exists route by
// route — just that the Server registers itself.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.FileServerFS(s.staticFS))
	mux.HandleFunc("GET /{$}", s.HandleIndex)
	mux.HandleFunc("GET /chart", s.HandleChart)
	mux.HandleFunc("GET /summary", s.HandleSummary)
	mux.HandleFunc("GET /healthz", s.HandleHealthz)
	mux.HandleFunc("GET /sw.js", s.HandleServiceWorker)
	mux.HandleFunc("GET /overnight", s.HandleOvernightTab)
	mux.HandleFunc("GET /overnight/windows", s.HandleOvernightWindows)
	mux.HandleFunc("GET /entries", s.HandleEntriesList)
	mux.HandleFunc("POST /entries", s.HandleCreate)
	mux.HandleFunc("GET /entries/{id}", s.HandleCancelEdit)
	mux.HandleFunc("GET /entries/{id}/edit", s.HandleEdit)
	mux.HandleFunc("PUT /entries/{id}", s.HandleUpdate)
	mux.HandleFunc("DELETE /entries/{id}", s.HandleDelete)
	mux.HandleFunc("POST /goals", s.HandleGoalCreate)
	mux.HandleFunc("GET /goals/{id}", s.HandleGoalCancelEdit)
	mux.HandleFunc("GET /goals/{id}/edit", s.HandleGoalEdit)
	mux.HandleFunc("PUT /goals/{id}", s.HandleGoalUpdate)
	mux.HandleFunc("DELETE /goals/{id}", s.HandleGoalDelete)
	mux.HandleFunc("POST /markers", s.HandleMarkerCreate)
	mux.HandleFunc("GET /markers/{id}", s.HandleMarkerCancelEdit)
	mux.HandleFunc("GET /markers/{id}/edit", s.HandleMarkerEdit)
	mux.HandleFunc("PUT /markers/{id}", s.HandleMarkerUpdate)
	mux.HandleFunc("DELETE /markers/{id}", s.HandleMarkerDelete)
	mux.HandleFunc("GET /export.csv", s.HandleExport)
	mux.HandleFunc("GET /backup.db", s.HandleBackup)
	mux.HandleFunc("GET /import", s.HandleImportForm)
	mux.HandleFunc("POST /import", s.HandleImport)
	mux.HandleFunc("POST /settings/delete-all", s.HandleDeleteAll)
}

// parseIDPath reads the {id} path value shared by every entity's
// edit/cancel-edit/update/delete routes.
func parseIDPath(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
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
