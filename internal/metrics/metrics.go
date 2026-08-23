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
	"time"
)

// buckets are upper bounds in seconds, matching Prometheus's own default
// client histogram buckets so dashboards built against other apps' metrics
// still make sense against this one.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type key struct {
	method string
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
	mu    sync.Mutex
	hists = map[key]*histogram{}
)

// Instrument wraps h, recording a request-duration histogram labeled by
// method and status code for every request it serves.
func Instrument(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r)
		record(r.Method, sw.status, time.Since(start).Seconds())
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

func record(method string, status int, seconds float64) {
	mu.Lock()
	defer mu.Unlock()
	k := key{method: method, status: status}
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

// Handler renders every recorded histogram in Prometheus text exposition
// format.
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
			if keys[i].method != keys[j].method {
				return keys[i].method < keys[j].method
			}
			return keys[i].status < keys[j].status
		})

		for _, k := range keys {
			h := hists[k]
			labels := fmt.Sprintf("method=%q,status=%q", k.method, strconv.Itoa(k.status))
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
	})
}
