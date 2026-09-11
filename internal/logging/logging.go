// Package logging installs the process-wide slog handler: structured JSON on
// stdout, with the current span's trace and span ids attached to every record
// that carries a context.
//
// The JSON matters because of where these lines end up. Alloy tails the
// container's stdout and ships it to Loki, and a matching stage.json in that
// pipeline promotes these fields to queryable ones. A formatted sentence is a
// string Loki can only regex; a static message plus attributes is something
// it can group by. That is the whole reason call sites say
// slog.ErrorContext(ctx, "render page", "page", page, "error", err) rather
// than printing a sentence.
//
// The trace ids matter because traces already go to Tempo (internal/tracing)
// and logs already go to Loki, and without a shared id there is no way to get
// from one to the other. With trace_id on the log line, Grafana's Loki-to-Tempo
// correlation turns "this request 500'd" into the span tree that produced it.
//
// What must never go through here: user identifiers and user content. Every
// line written by this package leaves the cluster and lands in Grafana Cloud
// with Loki's retention, which is a different exposure from a process's own
// stderr — so no authenticated email, no session or CSRF value, no Access
// JWT, no request headers or bodies, and none of whatever the app actually
// stores. An attribute naming the field that failed is diagnostic; the value
// in it usually is not, and errors carry enough on their own. When a call
// site would need a user's data to be debuggable, the trace is where that
// belongs, not the log.
package logging

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// Init makes slog's package-level functions write JSON to stdout at Info and
// above, through the trace-aware handler below. Call it first in main, before
// anything that might log: until it runs, slog's default handler is writing
// unstructured text to stderr, which Alloy will ship but nothing downstream
// can parse.
func Init() {
	slog.SetDefault(slog.New(&traceHandler{
		Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	}))
}

// traceHandler decorates records with the span context found on the context
// they were logged with.
//
// This is a handler rather than something each call site does for itself
// because a call site that forgets is silently useless — the line still
// appears in Loki, it just can't be joined to anything, and nobody notices
// until they need the trace. Doing it here means the only thing a call site
// has to get right is passing a real context, which is visible in the
// InfoContext/ErrorContext name.
type traceHandler struct {
	slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	// Only a valid span context is worth attaching. An unsampled or absent
	// span yields all-zero ids, and emitting trace_id="00000000..." on every
	// startup and shutdown line would be worse than emitting nothing: it
	// looks like a trace id, so it invites a Tempo lookup that can only ever
	// come back empty.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must return a *traceHandler rather than the
// embedded handler's own result, which is what slog.Handler's embedding
// would otherwise give: slog.New(...).With(...) calls WithAttrs, and if that
// returned the bare JSON handler the trace ids would quietly stop appearing
// on every logger derived that way.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
