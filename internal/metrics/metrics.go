// Package metrics exposes a Prometheus /metrics endpoint and an HTTP
// middleware that records request latency and concurrency.
//
// The metric contract is frozen.
// resources/dashboards/jcwpi-observability.json and
// resources/alerts/app-error-burst.yaml in the homelab repo build one panel
// and one alert expression per app out of these exact series,
// so a renamed metric or a renamed label produces an empty panel rather than
// an error anyone would notice.
// The names below, the label set, the order of the labels and the bucket
// bounds are therefore not this app's to change on its own:
// all seven apps move together or not at all.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// status is a string label holding the numeric code — not an int,
// and not named "code" — because that is what the hand-rolled exposition
// this replaced emitted, and what the dashboard queries match on.
//
// prometheus.DefBuckets is byte-identical to the bucket list that used to be
// spelled out here, so it is referenced rather than restated:
// these are the client-library defaults every app's dashboard is already
// built against.
var (
	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})

	inFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "http_requests_in_flight",
		Help: "HTTP requests currently being served.",
	})
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
		inFlight.Inc()
		defer inFlight.Dec()

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
		duration.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).
			Observe(time.Since(start).Seconds())
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

// Unwrap is how net/http's ResponseController reaches the real
// ResponseWriter through this wrapper.
// Embedding http.ResponseWriter forwards only the three interface methods;
// the optional ones a handler might need — Flush, Hijack, the read and
// write deadline setters — are found by unwrapping,
// and without this method http.NewResponseController stops at statusWriter
// and reports every one of them as unsupported.
// The failure that would cause is silent: a streaming or hijacking handler
// keeps compiling and starts returning errors at runtime instead.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Handler serves the default registry: the app's own two series above,
// plus the Go runtime and process collectors client_golang registers there
// by default, which is where go_goroutines and the rest come from.
func Handler() http.Handler { return promhttp.Handler() }
