// Package metrics exposes a Prometheus /metrics endpoint and an HTTP
// middleware recording request latency and concurrency.
//
// Storage and rendering are prometheus/client_golang's. This package owns
// only the route-label decision, which no client library can make for us.
//
// The metric contract is frozen: jchevertonwynne/homelab builds one dashboard
// panel per app, plus the AppErrorBurst alert, from these exact series names,
// label names and label order. A rename produces an empty panel rather than
// an error. So status stays a string label holding the numeric code (not
// "code"), the labels stay in method/route/status order, and the buckets stay
// DefBuckets.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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

// PatternHandler is the one method of *http.ServeMux this package needs: the
// matched route pattern ("GET /entries/{id}"), not the raw path
// ("GET /entries/42"). Labelling by raw path would mean one series per row
// ever created, so this is what makes per-route latency affordable.
type PatternHandler interface {
	Handler(r *http.Request) (http.Handler, string)
}

type config struct {
	routers  []PatternHandler
	excluded map[string]bool
}

type Option func(*config)

// WithRouters adds routers consulted for the route label after h itself,
// later ones winning. Only needed for a layered setup: an outer mux with a
// "/" catch-all wrapping an inner mux whose patterns are the real endpoints,
// behind middleware the outer mux cannot see through.
func WithRouters(routers ...PatternHandler) Option {
	return func(c *config) { c.routers = append(c.routers, routers...) }
}

// WithExcludedRoutes keeps the named route patterns out of both the histogram
// and the in-flight gauge. For long-lived streams: an SSE connection open for
// hours is one request by HTTP's accounting, and would make the latency
// histogram and the gauge lie for as long as it lasts.
func WithExcludedRoutes(routes ...string) Option {
	return func(c *config) {
		if c.excluded == nil {
			c.excluded = make(map[string]bool, len(routes))
		}
		for _, r := range routes {
			c.excluded[r] = true
		}
	}
}

// Instrument wraps h. Anything no router matched is labelled "unmatched"
// rather than the raw path, for the same cardinality reason as PatternHandler.
func Instrument(h http.Handler, opts ...Option) http.Handler {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := ""
		if router, ok := h.(PatternHandler); ok {
			if _, pattern := router.Handler(r); pattern != "" {
				route = pattern
			}
		}
		for _, router := range cfg.routers {
			if _, pattern := router.Handler(r); pattern != "" {
				route = pattern
			}
		}
		if route == "" {
			route = "unmatched"
		}

		// The route has to be known before the gauge is touched, so an
		// excluded route never increments it.
		if cfg.excluded[route] {
			h.ServeHTTP(w, r)
			return
		}

		inFlight.Inc()
		defer inFlight.Dec()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r)
		duration.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).
			Observe(time.Since(start).Seconds())
	})
}

// statusWriter captures the status a handler wrote, defaulting to 200 because
// Write sends that implicitly when WriteHeader is never called.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.NewResponseController see through to the real writer.
// Without it, embedding the interface hides Flush from ResponseController and
// it returns ErrNotSupported — silently, so a stream just never flushes.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Handler serves the default registry: these metrics plus the Go runtime and
// process collectors client_golang registers there for free.
func Handler() http.Handler { return promhttp.Handler() }
