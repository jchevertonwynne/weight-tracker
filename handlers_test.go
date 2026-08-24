package main

import (
	"bytes"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"weight-tracker/internal/db"
	"weight-tracker/internal/handlers"
)

// testServer bundles a *handlers.Server with the raw *sql.DB it was built
// from, so tests can both drive HTTP handlers (via the embedded Server)
// and assert on stored state directly (via db.ListEntries(t.Context(), s.db) etc.) —
// handlers.Server's own db field is private to its package.
type testServer struct {
	*handlers.Server
	db *sql.DB
}

// newTestServer builds a server backed by a throwaway on-disk database.
// t.TempDir is cleaned up automatically, so each test gets a fresh schema.
// Uses the same templatesFS/staticFS main.go embeds, and the real
// time.Now — a test that needs a fixed clock instead can call
// handlers.New directly.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return &testServer{Server: handlers.New(sqlDB, templatesFS, staticFS, time.Now), db: sqlDB}
}

// postForm runs a urlencoded form POST/PUT through the handler under test.
func postForm(t *testing.T, h http.HandlerFunc, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// entryForm is a valid weigh-in submission, for tests that vary one field.
func entryForm(weightKg string) url.Values {
	return url.Values{
		"weight_kg":        {weightKg},
		"recorded_at_date": {"2026-08-16"},
		"recorded_at_time": {"07:30"},
		"period_override":  {""},
	}
}

func TestHandleCreateRejectsInvalidWeights(t *testing.T) {
	// Regression: the form's min/step attributes are client-side only, so a
	// direct POST could previously store a negative or non-finite weight.
	tests := []struct {
		name   string
		weight string
	}{
		{"negative", "-50"},
		{"zero", "0"},
		{"not a number", "abc"},
		{"empty", ""},
		{"absurdly large", "5000"},
		{"NaN", "NaN"},
		{"positive infinity", "Inf"},
		{"negative infinity", "-Inf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm(tc.weight))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			entries, err := db.ListEntries(t.Context(), s.db)
			if err != nil {
				t.Fatalf("list entries: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("stored %d entries, want none", len(entries))
			}
		})
	}
}

func TestHandleCreateAcceptsValidWeights(t *testing.T) {
	for _, weight := range []string{"82.4", "0.1", "1000"} {
		t.Run(weight, func(t *testing.T) {
			s := newTestServer(t)
			rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm(weight))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
			}
			entries, err := db.ListEntries(t.Context(), s.db)
			if err != nil {
				t.Fatalf("list entries: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("stored %d entries, want 1", len(entries))
			}
		})
	}
}

func TestHandleCreateRejectsInvalidPeriodOverride(t *testing.T) {
	s := newTestServer(t)
	form := entryForm("82.4")
	form.Set("period_override", "afternoon")
	rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleCreateRejectsInvalidTimestamp(t *testing.T) {
	s := newTestServer(t)
	form := entryForm("82.4")
	form.Set("recorded_at_date", "not-a-date")
	rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGoalCreateRejectsInvalidWeights(t *testing.T) {
	for _, weight := range []string{"-10", "0", "abc", "NaN"} {
		t.Run(weight, func(t *testing.T) {
			s := newTestServer(t)
			form := url.Values{"weight_kg": {weight}, "effective_from": {"2026-08-01"}}
			rec := postForm(t, s.HandleGoalCreate, http.MethodPost, "/goals", form)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			goals, err := db.ListGoals(t.Context(), s.db)
			if err != nil {
				t.Fatalf("list goals: %v", err)
			}
			if len(goals) != 0 {
				t.Errorf("stored %d goals, want none", len(goals))
			}
		})
	}
}

func TestHandleMarkerCreateRequiresANote(t *testing.T) {
	for _, note := range []string{"", "   "} {
		s := newTestServer(t)
		form := url.Values{"date": {"2026-08-10"}, "note": {note}}
		rec := postForm(t, s.HandleMarkerCreate, http.MethodPost, "/markers", form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("note %q: status = %d, want 400", note, rec.Code)
		}
	}
}

// deleteRequest issues a DELETE with the {id} path value the mux would set.
func deleteRequest(t *testing.T, h http.HandlerFunc, target, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, target, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestDeleteMissingRowReturns404(t *testing.T) {
	// Regression: deleting an already-deleted id used to report success.
	tests := []struct {
		name   string
		handle func(*testServer) http.HandlerFunc
		target string
	}{
		{"entry", func(s *testServer) http.HandlerFunc { return s.HandleDelete }, "/entries/999"},
		{"goal", func(s *testServer) http.HandlerFunc { return s.HandleGoalDelete }, "/goals/999"},
		{"marker", func(s *testServer) http.HandlerFunc { return s.HandleMarkerDelete }, "/markers/999"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := deleteRequest(t, tc.handle(s), tc.target, "999")
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestDeleteExistingEntryReturns200AndRemovesIt(t *testing.T) {
	s := newTestServer(t)
	rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("82.4"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	entries, err := db.ListEntries(t.Context(), s.db)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one entry, got %v (err %v)", entries, err)
	}

	id := entries[0].ID
	rec = deleteRequest(t, s.HandleDelete, "/entries/1", "1")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	remaining, err := db.ListEntries(t.Context(), s.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("entry %d survived the delete", id)
	}

	// Deleting the same id a second time is now a 404, not a silent success.
	rec = deleteRequest(t, s.HandleDelete, "/entries/1", "1")
	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", rec.Code)
	}
}

func TestDeleteRejectsNonNumericID(t *testing.T) {
	s := newTestServer(t)
	rec := deleteRequest(t, s.HandleDelete, "/entries/abc", "abc")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// uploadCSV builds a multipart import request the way the import form does.
func uploadCSV(t *testing.T, csv, unit string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "weights.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(csv)); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	if err := mw.WriteField("unit", unit); err != nil {
		t.Fatalf("write unit field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestHandleImportAcceptsEveryUnitTheFormOffers(t *testing.T) {
	// Regression: the form offered "grams" while the handler accepted only
	// kg and lb, so choosing it failed with a 400.
	// Expectations are exact gram counts — storing whole grams means an
	// import no longer lands on an approximate value needing a tolerance.
	tests := []struct {
		unit  string
		csv   string
		wantG int64
	}{
		{"kg", "recorded_at,weight\n2026-08-14T07:00:00Z,81.5\n", 81500},
		{"lb", "recorded_at,weight\n2026-08-14T07:00:00Z,180\n", 81647}, // 180 lb = 81.6466266 kg
		{"g", "recorded_at,weight\n2026-08-14T07:00:00Z,81500\n", 81500},
	}
	for _, tc := range tests {
		t.Run(tc.unit, func(t *testing.T) {
			s := newTestServer(t)
			rec := httptest.NewRecorder()
			s.HandleImport(rec, uploadCSV(t, tc.csv, tc.unit))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
			}
			entries, err := db.ListEntries(t.Context(), s.db)
			if err != nil {
				t.Fatalf("list entries: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("stored %d entries, want 1", len(entries))
			}
			if entries[0].WeightG != tc.wantG {
				t.Errorf("stored %v g, want %v", entries[0].WeightG, tc.wantG)
			}
		})
	}
}

func TestHandleImportRejectsUnknownUnit(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.HandleImport(rec, uploadCSV(t, "recorded_at,weight\n2026-08-14T07:00:00Z,81.5\n", "stone"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleImportIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	csv := "recorded_at,weight_kg\n2026-08-14T07:00:00Z,81.5\n2026-08-15T07:00:00Z,81.2\n"

	for i := range 2 {
		rec := httptest.NewRecorder()
		s.HandleImport(rec, uploadCSV(t, csv, "kg"))
		if rec.Code != http.StatusOK {
			t.Fatalf("import %d: status = %d", i+1, rec.Code)
		}
	}
	entries, err := db.ListEntries(t.Context(), s.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("stored %d entries after importing the same file twice, want 2", len(entries))
	}
}

func TestHandleExportRoundTripsThroughImport(t *testing.T) {
	s := newTestServer(t)
	created := []struct{ weight, time string }{
		{"82.4", "07:30"},
		{"81.9", "21:15"},
	}
	for _, c := range created {
		form := entryForm(c.weight)
		form.Set("recorded_at_time", c.time)
		if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", form); rec.Code != http.StatusOK {
			t.Fatalf("create %s at %s failed: %d", c.weight, c.time, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	s.HandleExport(rec, httptest.NewRequest(http.MethodGet, "/export.csv", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	exported := rec.Body.String()
	if !strings.HasPrefix(exported, "recorded_at,weight_kg\n") {
		t.Fatalf("export header = %q", strings.SplitN(exported, "\n", 2)[0])
	}

	// Feed the export straight back into a fresh database.
	fresh := newTestServer(t)
	importRec := httptest.NewRecorder()
	fresh.HandleImport(importRec, uploadCSV(t, exported, "kg"))
	if importRec.Code != http.StatusOK {
		t.Fatalf("re-import status = %d (body: %s)", importRec.Code, importRec.Body)
	}
	reimported, err := db.ListEntries(t.Context(), fresh.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	original, err := db.ListEntries(t.Context(), s.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(reimported) != len(original) {
		t.Fatalf("re-imported %d entries, want %d", len(reimported), len(original))
	}
	for i := range original {
		if original[i].WeightG != reimported[i].WeightG {
			t.Errorf("entry %d: %v g became %v g", i, original[i].WeightG, reimported[i].WeightG)
		}
		if !original[i].RecordedAt.Equal(reimported[i].RecordedAt) {
			t.Errorf("entry %d: %v became %v", i, original[i].RecordedAt, reimported[i].RecordedAt)
		}
	}
}

func TestHandleIndexRenders(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.HandleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "82.4 kg") {
		t.Error("rendered page does not show the entry's weight")
	}
	if !strings.Contains(body, "Weight Tracker") {
		t.Error("rendered page is missing its title")
	}
}

func TestHandleChartReturnsJSON(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.HandleChart(rec, httptest.NewRequest(http.MethodGet, "/chart?range=all&series=all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"hasData":true`) {
		t.Errorf("body = %s, want hasData true", rec.Body)
	}
}

func TestHandleDeleteAllClearsEverything(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create entry failed: %d", rec.Code)
	}
	goalForm := url.Values{"weight_kg": {"78"}, "effective_from": {"2026-08-01"}}
	if rec := postForm(t, s.HandleGoalCreate, http.MethodPost, "/goals", goalForm); rec.Code != http.StatusOK {
		t.Fatalf("create goal failed: %d", rec.Code)
	}
	markerForm := url.Values{"date": {"2026-08-10"}, "note": {"started"}}
	if rec := postForm(t, s.HandleMarkerCreate, http.MethodPost, "/markers", markerForm); rec.Code != http.StatusOK {
		t.Fatalf("create marker failed: %d", rec.Code)
	}

	rec := postForm(t, s.HandleDeleteAll, http.MethodPost, "/settings/delete-all", nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}

	entries, _ := db.ListEntries(t.Context(), s.db)
	goals, _ := db.ListGoals(t.Context(), s.db)
	markers, _ := db.ListMarkers(t.Context(), s.db)
	if len(entries) != 0 || len(goals) != 0 || len(markers) != 0 {
		t.Errorf("after delete-all: %d entries, %d goals, %d markers; want none", len(entries), len(goals), len(markers))
	}
}

func TestHandleBackupServesARestorableDatabase(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create entry failed: %d", rec.Code)
	}
	markerForm := url.Values{"date": {"2026-08-10"}, "note": {"started cutting"}}
	if rec := postForm(t, s.HandleMarkerCreate, http.MethodPost, "/markers", markerForm); rec.Code != http.StatusOK {
		t.Fatalf("create marker failed: %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	s.HandleBackup(rec, httptest.NewRequest(http.MethodGet, "/backup.db", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.sqlite3" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".db") {
		t.Errorf("Content-Disposition = %q, want a .db attachment", cd)
	}
	body := rec.Body.Bytes()
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, but body is %d bytes", got, len(body))
	}
	// Every SQLite file starts with this header string.
	if !bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
		t.Fatalf("body is not a SQLite database (first bytes: %q)", body[:min(16, len(body))])
	}

	// The real test: write the downloaded bytes out and open them as a
	// database, the way restoring from this file would.
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restoredPath, body, 0o644); err != nil {
		t.Fatalf("write downloaded bytes: %v", err)
	}
	restored, err := db.Open(restoredPath)
	if err != nil {
		t.Fatalf("open the downloaded database: %v", err)
	}
	defer restored.Close()

	entries, err := db.ListEntries(t.Context(), restored)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 || entries[0].WeightG != 82400 {
		t.Errorf("restored entries = %+v, want one 82400 g entry", entries)
	}
	markers, err := db.ListMarkers(t.Context(), restored)
	if err != nil {
		t.Fatalf("list markers: %v", err)
	}
	if len(markers) != 1 || markers[0].Note != "started cutting" {
		t.Errorf("restored markers = %+v, want the marker to survive", markers)
	}
}

func TestHandleBackupOnAnEmptyDatabase(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.HandleBackup(rec, httptest.NewRequest(http.MethodGet, "/backup.db", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("SQLite format 3\x00")) {
		t.Error("empty-database backup is not a SQLite file")
	}
}

func TestHandleBackupCleansUpItsStagingFile(t *testing.T) {
	// The snapshot is written to a temp dir and streamed; nothing should be
	// left behind once the response is done.
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "weight-tracker-backup*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.HandleBackup(rec, httptest.NewRequest(http.MethodGet, "/backup.db", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "weight-tracker-backup*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("left %d staging directories behind: %v", len(after)-len(before), after)
	}
}

func TestHandleBackupDoesNotDisturbTheLiveDatabase(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.HandleBackup(rec, httptest.NewRequest(http.MethodGet, "/backup.db", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup status = %d", rec.Code)
	}
	// Writes and reads still work afterwards.
	form := entryForm("81.9")
	form.Set("recorded_at_time", "21:00")
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", form); rec.Code != http.StatusOK {
		t.Errorf("create after backup failed: %d", rec.Code)
	}
	entries, err := db.ListEntries(t.Context(), s.db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("live database holds %d entries after backup, want 2", len(entries))
	}
}

func TestHandleServiceWorker(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.HandleServiceWorker(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q, want application/javascript", ct)
	}
	// The worker script is the one thing that must never be served from a
	// stale cache: it is what teaches an already-installed browser the new
	// caching strategy. Lose this header and existing installs can stay
	// pinned to an old worker, which is how a stale stylesheet survived a
	// deploy before.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	body := rec.Body.String()
	// Served from the top level, not /static/, so its scope covers the whole
	// app rather than just /static/.
	if !strings.Contains(body, "addEventListener('fetch'") {
		t.Error("body does not look like the service worker")
	}
	// Guard the fix itself: a cache-first strategy here means CSS and htmx
	// changes never reach an installed browser.
	if !strings.Contains(body, "fetch(event.request)") {
		t.Error("service worker does not appear to try the network first")
	}
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.HandleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("body = %q, want it to say ok", rec.Body.String())
	}
}

// The index used to write the template straight to the ResponseWriter, which
// commits a 200 on the first byte; a template failing halfway then left
// http.Error setting a 500 on a committed response, and the client kept a
// truncated 200. Rendering into a buffer first means either the whole page or
// a clean error, never half of one.
func TestIndexDoesNotWriteAPartialPage(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.HandleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("response does not end with </html>; a partial page was sent:\n...%s", body[max(0, len(body)-200):])
	}
}
