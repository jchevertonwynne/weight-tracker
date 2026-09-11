package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"weight-tracker/internal/db"
)

// The attack refuseCrossOrigin exists for: a form on another site posting to
// /settings/delete-all while the browser holds a valid Access cookie for this
// hostname. Nothing else in the app would stop it, because nothing else in the
// app checks who is calling.
func TestCrossSiteDeleteAllIsRefused(t *testing.T) {
	s := newTestServer(t)
	if rec := postForm(t, s.HandleCreate, http.MethodPost, "/entries", entryForm("80.5")); rec.Code >= 400 {
		t.Fatalf("creating the fixture entry: %d %s", rec.Code, rec.Body.String())
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	h := refuseCrossOrigin(mux)

	for _, tc := range []struct{ header, value string }{
		{"Sec-Fetch-Site", "cross-site"},
		// Another subdomain of jchevertonwynne.uk is the same site but not the
		// same origin, and must not count as this app.
		{"Sec-Fetch-Site", "same-site"},
		{"Origin", "https://evil.example"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/settings/delete-all", nil)
		req.Header.Set(tc.header, tc.value)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %s = %d, want 403", tc.header, tc.value, rec.Code)
		}
	}

	entries, err := db.ListEntries(t.Context(), s.db)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries left after refused cross-site deletes, want 1", len(entries))
	}

	// The app's own button is a same-origin form, and still works.
	req := httptest.NewRequest(http.MethodPost, "/settings/delete-all", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin delete-all = %d, want 303", rec.Code)
	}
	if entries, _ := db.ListEntries(t.Context(), s.db); len(entries) != 0 {
		t.Fatalf("%d entries left after a same-origin delete-all, want 0", len(entries))
	}
}
