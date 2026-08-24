// Package metrics exposes a minimal Prometheus-format /metrics endpoint and
// an HTTP middleware that records request latency, without pulling in a
// third-party client library — the whole app has one dependency
// (modernc.org/sqlite) and this keeps it that way.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// buckets are upper bounds in seconds, matching Prometheus's own default
// client histogram buckets so dashboards built against other apps' metrics
// still make sense against this one.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type key struct {
	method string
	route  string
	status int
}

// histogram counts, per bucket, requests whose latency fell at or below
// that bucket's upper bound. counts[len(buckets)] is the +Inf overflow
// bucket for anything slower than the largest named bucket.
type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

var (
	mu       sync.Mutex
	hists    = map[key]*histogram{}
	inFlight int64
)

// patternHandler is the one method of *http.ServeMux that Instrument needs:
// the matched route pattern (e.g. "GET /entries/{id}"), not the raw path
// (e.g. "GET /entries/42"). Labeling by raw path would give the histogram
// unbounded cardinality — one series per row ever created, not per
// endpoint — so this is what makes per-route latency possible at all.
type patternHandler interface {
	Handler(r *http.Request) (http.Handler, string)
}

// Instrument wraps h, recording a request-duration histogram labeled by
// method, route and status code, plus a gauge of requests currently in
// flight.
//
// The route label is taken from the first of h itself (if it's a
// *http.ServeMux, checked via patternHandler) and extraRouters that
// returns a non-empty pattern for the request, later entries overriding
// earlier ones. A single flat mux needs nothing extra: h supplies its own
// patterns directly. Passing more is only for a layered setup like list's,
// where an outer mux (unauthenticated routes plus a "/" catch-all) wraps
// an inner one (the real per-endpoint patterns, behind auth middleware the
// outer mux can't see through) — there, h is the outer mux and the inner
// one is passed as an extra router so its more specific pattern wins.
// Anything genuinely unmatched — a 404, a route no router recognises — is
// labeled "unmatched" rather than the raw path, for the same cardinality
// reason.
func Instrument(h http.Handler, extraRouters ...patternHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&inFlight, 1)
		defer atomic.AddInt64(&inFlight, -1)

		route := ""
		if router, ok := h.(patternHandler); ok {
			if _, pattern := router.Handler(r); pattern != "" {
				route = pattern
			}
		}
		for _, router := range extraRouters {
			if _, pattern := router.Handler(r); pattern != "" {
				route = pattern
			}
		}
		if route == "" {
			route = "unmatched"
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r)
		record(r.Method, route, sw.status, time.Since(start).Seconds())
	})
}

// statusWriter captures the status code a handler wrote, defaulting to 200
// since http.ResponseWriter.Write implicitly sends that status if the
// handler never calls WriteHeader itself.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func record(method, route string, status int, seconds float64) {
	mu.Lock()
	defer mu.Unlock()
	k := key{method: method, route: route, status: status}
	h, ok := hists[k]
	if !ok {
		h = &histogram{counts: make([]uint64, len(buckets)+1)}
		hists[k] = h
	}
	h.count++
	h.sum += seconds
	// SearchFloat64s returns the first bucket whose bound is >= seconds —
	// exactly the one bucket (of the fixed set) this observation belongs
	// to before cumulative sums are computed at render time. A value past
	// every named bucket lands on len(buckets), the +Inf slot.
	h.counts[sort.SearchFloat64s(buckets, seconds)]++
}

// Handler renders every recorded histogram, plus the in-flight gauge, in
// Prometheus text exposition format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request latency in seconds.")
		fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")

		keys := make([]key, 0, len(hists))
		for k := range hists {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].route != keys[j].route {
				return keys[i].route < keys[j].route
			}
			if keys[i].method != keys[j].method {
				return keys[i].method < keys[j].method
			}
			return keys[i].status < keys[j].status
		})

		for _, k := range keys {
			h := hists[k]
			labels := fmt.Sprintf("method=%q,route=%q,status=%q", k.method, k.route, strconv.Itoa(k.status))
			var cumulative uint64
			for i, b := range buckets {
				cumulative += h.counts[i]
				fmt.Fprintf(w, "http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(b, 'g', -1, 64), cumulative)
			}
			cumulative += h.counts[len(buckets)]
			fmt.Fprintf(w, "http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, cumulative)
			fmt.Fprintf(w, "http_request_duration_seconds_sum{%s} %s\n", labels, strconv.FormatFloat(h.sum, 'g', -1, 64))
			fmt.Fprintf(w, "http_request_duration_seconds_count{%s} %d\n", labels, h.count)
		}

		fmt.Fprintln(w, "# HELP http_requests_in_flight HTTP requests currently being served.")
		fmt.Fprintln(w, "# TYPE http_requests_in_flight gauge")
		fmt.Fprintf(w, "http_requests_in_flight %d\n", atomic.LoadInt64(&inFlight))
	})
}
