package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weight-tracker/internal/db"
)

func TestNewGrafanaProxyRejectsBadURLs(t *testing.T) {
	for _, target := range []string{"127.0.0.1:3000", "not a url", "/grafana"} {
		if _, err := newGrafanaProxy(target); err == nil {
			t.Errorf("target %q was accepted, want an error naming the missing scheme/host", target)
		}
	}
}

func TestGrafanaProxyForwardsThePathUnchanged(t *testing.T) {
	// Grafana runs with serve_from_sub_path, so it expects to see the
	// /grafana prefix itself — stripping it would break every asset URL.
	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "panel")
	}))
	defer backend.Close()

	proxy, err := newGrafanaProxy(backend.URL)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grafana/d-solo/weight-tracker/weight?panelId=2&theme=dark", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/grafana/d-solo/weight-tracker/weight" {
		t.Errorf("backend saw path %q, want the /grafana prefix preserved", gotPath)
	}
	if !strings.Contains(gotQuery, "panelId=2") || !strings.Contains(gotQuery, "theme=dark") {
		t.Errorf("backend saw query %q, want panelId and theme forwarded", gotQuery)
	}
	if rec.Body.String() != "panel" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestGrafanaProxyExplainsAnUnreachableBackend(t *testing.T) {
	// Port 1 on localhost refuses connections, standing in for a stopped
	// Grafana. The iframe should say what is wrong rather than going blank.
	proxy, err := newGrafanaProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grafana/d-solo/x", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Grafana is not responding") {
		t.Errorf("body does not explain the failure: %q", body)
	}
	if !strings.Contains(body, "still works") {
		t.Error("body does not reassure that the rest of the app is fine")
	}
}

func TestGraphsTabRendersWhenGrafanaIsEnabled(t *testing.T) {
	s := newTestServer(t)
	s.grafanaEnabled = true
	if rec := postForm(t, s.handleCreate, http.MethodPost, "/entries", entryForm("82.4")); rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`id="tab-graphs"`,
		`id="graph-frame"`,
		`data-tab="graphs"`,
		`window.EARLIEST_MS =`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	// The iframe must start blank; Grafana's bundle is only worth fetching
	// once the tab is opened.
	if !strings.Contains(body, `src="about:blank"`) {
		t.Error("iframe does not start blank, so Grafana loads on every page view")
	}
}

func TestGraphsTabExplainsItselfWhenGrafanaIsDisabled(t *testing.T) {
	s := newTestServer(t) // grafanaEnabled defaults to false
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if strings.Contains(body, `id="graph-frame"`) {
		t.Error("an iframe was rendered with no Grafana to point it at")
	}
	if !strings.Contains(body, "Grafana is not configured") {
		t.Error("the tab does not explain why it is empty")
	}
}

func TestEarliestMs(t *testing.T) {
	now := at(t, "2026-08-16 12:00")

	if got := earliestMs(nil, now); got != now.UnixMilli() {
		t.Errorf("with no entries, earliestMs = %d, want now (%d)", got, now.UnixMilli())
	}

	// Deliberately unsorted, since ListEntries returns newest-first and the
	// oldest is what an "All time" range needs.
	entries := []db.Entry{
		entry(2, at(t, "2026-08-16 07:00"), 82.0, ""),
		entry(1, at(t, "2026-08-14 07:00"), 84.0, ""),
		entry(3, at(t, "2026-08-15 07:00"), 83.0, ""),
	}
	if got, want := earliestMs(entries, now), at(t, "2026-08-14 07:00").UnixMilli(); got != want {
		t.Errorf("earliestMs = %d, want the oldest entry %d", got, want)
	}
}
