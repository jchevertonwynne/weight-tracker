// Package logging installs the process-wide slog handler: structured JSON on
// stdout, with the active trace and span ids attached to every record that
// carries a context.
//
// The shape is dictated by what reads it. Alloy tails the container's stdout
// and ships it to Loki with a stage.json, so a line has to be one JSON
// object per record for its fields to become queryable at all — a formatted
// English sentence from stdlib log arrives as an opaque blob.
// The trace_id and span_id fields are what let a Loki log line link to the
// Tempo trace it came from: Grafana's derived field matches on trace_id, so a
// 500 in the logs and the span that produced it are one click apart instead
// of a manual hunt through timestamps.
//
// Nothing here logs request content. This app stores personal health data
// (weights, goals, the times of overnight readings) and the repo is public,
// so log records carry errors and lifecycle events only — never an entry
// value, a date tied to a reading, or the authenticated user's identity.
package logging

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// Init points slog's default logger at stdout as JSON at LevelInfo.
// Debug is off: the Pi's disk is an SD card and Loki retention is finite, so
// the default is the level worth keeping rather than the level worth having
// available.
func Init() {
	slog.SetDefault(slog.New(&traceHandler{
		Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
	}))
}

// traceHandler decorates each record with the ids of the span its context
// belongs to.
//
// It has to be a handler rather than something each call site does, because
// the alternative is every log statement in the app remembering to pull the
// span context out by hand — which is exactly the kind of thing that gets
// forgotten in the handler where it would have mattered most.
type traceHandler struct {
	slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	// Only a valid span context is worth attaching. Tracing is disabled
	// entirely when no OTLP endpoint is configured (local dev, and the
	// cluster before Alloy is up), and outside a request there is no span
	// at all — in both cases the ids would be all-zero, which looks like a
	// real trace id in Loki and links to nothing in Tempo.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup have to be overridden so that slog.With and
// slog.WithGroup return a logger that still goes through Handle above.
// The embedded Handler's own implementations return the inner JSON handler
// naked, silently dropping the trace ids from everything derived from it.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
