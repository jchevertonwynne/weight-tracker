package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstrumentRecordsStatusAndCount(t *testing.T) {
	// hists is package-level state; give this test its own key so it can't
	// collide with counts left behind by other tests in the package.
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest("PROPFIND", "/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}

	body := renderMetrics(t)
	wantLine := `http_request_duration_seconds_count{method="PROPFIND",status="418"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q; got:\n%s", wantLine, body)
	}
}

func TestInstrumentDefaultsStatusTo200(t *testing.T) {
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler never calls WriteHeader — net/http implicitly sends 200
		// on the first Write, and statusWriter must default the same way.
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("TRACE", "/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := renderMetrics(t)
	wantLine := `http_request_duration_seconds_count{method="TRACE",status="200"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q; got:\n%s", wantLine, body)
	}
}

func renderMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
