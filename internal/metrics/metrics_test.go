package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// What is worth testing here is only what this package still decides for
// itself: which route label a request gets, that an unroutable one is
// bounded to "unmatched", that a later extraRouter wins, that the status
// defaults to 200, that the gauge tracks a request's lifetime, and that
// statusWriter stays transparent to ResponseController.
// Rendering Prometheus text exposition is client_golang's job now, so the
// tests that asserted the shape of that output went with the code that
// produced it.

func TestInstrumentRecordsStatusAndCount(t *testing.T) {
	// duration and inFlight are package-level state shared by every test in
	// the package; each test uses a method it alone uses so its series
	// cannot collide with observations another test left behind.
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest("PROPFIND", "/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}

	// A plain http.HandlerFunc (not a *http.ServeMux) can't offer a route
	// pattern, so this falls back to "unmatched" rather than the raw path.
	if got := observations(t, "PROPFIND", "unmatched", "418"); got != 1 {
		t.Fatalf("observations for PROPFIND/unmatched/418 = %d, want 1", got)
	}
}

func TestInstrumentDefaultsStatusTo200(t *testing.T) {
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler never calls WriteHeader — net/http implicitly sends 200
		// on the first Write, and statusWriter must default the same way.
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodTrace, "/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := observations(t, http.MethodTrace, "unmatched", "200"); got != 1 {
		t.Fatalf("observations for TRACE/unmatched/200 = %d, want 1", got)
	}
}

func TestInstrumentLabelsByMuxPattern(t *testing.T) {
	// A *http.ServeMux passed directly satisfies patternHandler itself, so
	// no extraRouters are needed to get the matched pattern rather than the
	// raw path.
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := Instrument(mux)

	req := httptest.NewRequest(http.MethodOptions, "/widgets/42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// The raw path would have made one series per widget id ever visited.
	if got := observations(t, http.MethodOptions, "OPTIONS /widgets/{id}", "200"); got != 1 {
		t.Fatalf("observations for the matched pattern = %d, want 1", got)
	}
}

func TestInstrumentFallsBackToExtraRouterPattern(t *testing.T) {
	// Mirrors list's layered setup: an outer mux only knows a "/" catch-all
	// for authenticated routes, and the real per-endpoint pattern lives on
	// an inner mux the outer one wraps behind other middleware. The inner
	// mux, passed as an extraRouter, should win over the outer's "/".
	inner := http.NewServeMux()
	inner.HandleFunc("DELETE /collections/{collection}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	outer := http.NewServeMux()
	outer.Handle("/", inner)

	handler := Instrument(outer, inner)

	req := httptest.NewRequest(http.MethodDelete, "/collections/groceries", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := observations(t, http.MethodDelete, "DELETE /collections/{collection}", "200"); got != 1 {
		t.Fatalf("observations for the inner mux's pattern = %d, want 1", got)
	}
}

func TestInstrumentTracksInFlightForTheRequestsDuration(t *testing.T) {
	// The gauge has to go up before the wrapped handler runs and come back
	// down after it returns, which is only observable from inside the
	// handler: a before/after reading either side of ServeHTTP would be
	// equal even if both the Inc and the Dec were missing.
	before := testutil.ToFloat64(inFlight)

	var during float64
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during = testutil.ToFloat64(inFlight)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("REPORT", "/whatever", nil))

	if during != before+1 {
		t.Fatalf("in-flight during request = %v, want %v", during, before+1)
	}
	if after := testutil.ToFloat64(inFlight); after != before {
		t.Fatalf("in-flight after request = %v, want %v", after, before)
	}
}

func TestStatusWriterUnwrapsForResponseController(t *testing.T) {
	// statusWriter embeds http.ResponseWriter, which forwards only the three
	// interface methods; Flush, Hijack and the deadline setters are reached
	// by unwrapping. Without statusWriter.Unwrap, ResponseController stops
	// at the wrapper and reports all of them unsupported, which breaks a
	// streaming handler at runtime with nothing failing at compile time.
	var flushErr error
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("chunk"))
		flushErr = http.NewResponseController(w).Flush()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("SEARCH", "/whatever", nil))

	if flushErr != nil {
		t.Fatalf("Flush through statusWriter: %v (unsupported, meaning Unwrap is missing: %v)",
			flushErr, errors.Is(flushErr, http.ErrNotSupported))
	}
}

// observations returns how many samples the duration histogram holds for
// exactly this label set. Reading the collected metric rather than the
// rendered text keeps these tests about label selection, which is this
// package's decision, and off exposition format, which is not.
func observations(t *testing.T, method, route, status string) uint64 {
	t.Helper()
	obs, err := duration.GetMetricWithLabelValues(method, route, status)
	if err != nil {
		t.Fatalf("get histogram for %s/%s/%s: %v", method, route, status, err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram for %s/%s/%s: %v", method, route, status, err)
	}
	return m.GetHistogram().GetSampleCount()
}
