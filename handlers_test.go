package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"weight-tracker/internal/db"
)

// newTestServer builds a server backed by a throwaway on-disk database.
// t.TempDir is cleaned up automatically, so each test gets a fresh schema.
func newTestServer(t *testing.T) *server {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return &server{db: sqlDB}
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
			rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm(tc.weight))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			entries, err := db.ListEntries(s.db)
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
			rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm(weight))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
			}
			entries, err := db.ListEntries(s.db)
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
	rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleCreateRejectsInvalidTimestamp(t *testing.T) {
	s := newTestServer(t)
	form := entryForm("82.4")
	form.Set("recorded_at_date", "not-a-date")
	rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGoalCreateRejectsInvalidWeights(t *testing.T) {
	for _, weight := range []string{"-10", "0", "abc", "NaN"} {
		t.Run(weight, func(t *testing.T) {
			s := newTestServer(t)
			form := url.Values{"weight_kg": {weight}, "effective_from": {"2026-08-01"}}
			rec := postForm(t, s.handleGoalCreate, http.MethodPost, "/goals", form)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			goals, err := db.ListGoals(s.db)
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
		rec := postForm(t, s.handleMarkerCreate, http.MethodPost, "/markers", form)
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
		handle func(*server) http.HandlerFunc
		target string
	}{
		{"entry", func(s *server) http.HandlerFunc { return s.handleDelete }, "/entries/999"},
		{"goal", func(s *server) http.HandlerFunc { return s.handleGoalDelete }, "/goals/999"},
		{"marker", func(s *server) http.HandlerFunc { return s.handleMarkerDelete }, "/markers/999"},
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
	rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm("82.4"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	entries, err := db.ListEntries(s.db)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one entry, got %v (err %v)", entries, err)
	}

	id := entries[0].ID
	rec = deleteRequest(t, s.handleDelete, "/entries/1", "1")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	remaining, err := db.ListEntries(s.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("entry %d survived the delete", id)
	}

	// Deleting the same id a second time is now a 404, not a silent success.
	rec = deleteRequest(t, s.handleDelete, "/entries/1", "1")
	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", rec.Code)
	}
}

func TestDeleteRejectsNonNumericID(t *testing.T) {
	s := newTestServer(t)
	rec := deleteRequest(t, s.handleDelete, "/entries/abc", "abc")
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
	tests := []struct {
		unit      string
		csv       string
		wantKg    float64
		tolerance float64
	}{
		{"kg", "recorded_at,weight\n2026-08-14T07:00:00Z,81.5\n", 81.5, 0.001},
		{"lb", "recorded_at,weight\n2026-08-14T07:00:00Z,180\n", 81.6466, 0.001},
		{"g", "recorded_at,weight\n2026-08-14T07:00:00Z,81500\n", 81.5, 0.001},
	}
	for _, tc := range tests {
		t.Run(tc.unit, func(t *testing.T) {
			s := newTestServer(t)
			rec := httptest.NewRecorder()
			s.handleImport(rec, uploadCSV(t, tc.csv, tc.unit))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
			}
			entries, err := db.ListEntries(s.db)
			if err != nil {
				t.Fatalf("list entries: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("stored %d entries, want 1", len(entries))
			}
			if diff := entries[0].WeightKg - tc.wantKg; diff > tc.tolerance || diff < -tc.tolerance {
				t.Errorf("stored %v kg, want ~%v", entries[0].WeightKg, tc.wantKg)
			}
		})
	}
}

func TestHandleImportRejectsUnknownUnit(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleImport(rec, uploadCSV(t, "recorded_at,weight\n2026-08-14T07:00:00Z,81.5\n", "stone"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleImportIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	csv := "recorded_at,weight_kg\n2026-08-14T07:00:00Z,81.5\n2026-08-15T07:00:00Z,81.2\n"

	for i := range 2 {
		rec := httptest.NewRecorder()
		s.handleImport(rec, uploadCSV(t, csv, "kg"))
		if rec.Code != http.StatusOK {
			t.Fatalf("import %d: status = %d", i+1, rec.Code)
		}
	}
	entries, err := db.ListEntries(s.db)
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
		if rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", form); rec.Code != http.StatusOK {
			t.Fatalf("create %s at %s failed: %d", c.weight, c.time, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	s.handleExport(rec, httptest.NewRequest(http.MethodGet, "/export.csv", nil))
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
	fresh.handleImport(importRec, uploadCSV(t, exported, "kg"))
	if importRec.Code != http.StatusOK {
		t.Fatalf("re-import status = %d (body: %s)", importRec.Code, importRec.Body)
	}
	reimported, err := db.ListEntries(fresh.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	original, err := db.ListEntries(s.db)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(reimported) != len(original) {
		t.Fatalf("re-imported %d entries, want %d", len(reimported), len(original))
	}
	for i := range original {
		if !nearlyEqual(original[i].WeightKg, reimported[i].WeightKg) {
			t.Errorf("entry %d: %v kg became %v kg", i, original[i].WeightKg, reimported[i].WeightKg)
		}
		if !original[i].RecordedAt.Equal(reimported[i].RecordedAt) {
			t.Errorf("entry %d: %v became %v", i, original[i].RecordedAt, reimported[i].RecordedAt)
		}
	}
}

func TestHandleIndexRenders(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
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
	if rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.handleChart(rec, httptest.NewRequest(http.MethodGet, "/chart?range=all&series=all", nil))
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
	if rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create entry failed: %d", rec.Code)
	}
	goalForm := url.Values{"weight_kg": {"78"}, "effective_from": {"2026-08-01"}}
	if rec := postForm(t, s.handleGoalCreate, http.MethodPost, "/goals", goalForm); rec.Code != http.StatusOK {
		t.Fatalf("create goal failed: %d", rec.Code)
	}
	markerForm := url.Values{"date": {"2026-08-10"}, "note": {"started"}}
	if rec := postForm(t, s.handleMarkerCreate, http.MethodPost, "/markers", markerForm); rec.Code != http.StatusOK {
		t.Fatalf("create marker failed: %d", rec.Code)
	}

	rec := postForm(t, s.handleDeleteAll, http.MethodPost, "/settings/delete-all", nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}

	entries, _ := db.ListEntries(s.db)
	goals, _ := db.ListGoals(s.db)
	markers, _ := db.ListMarkers(s.db)
	if len(entries) != 0 || len(goals) != 0 || len(markers) != 0 {
		t.Errorf("after delete-all: %d entries, %d goals, %d markers; want none", len(entries), len(goals), len(markers))
	}
}
