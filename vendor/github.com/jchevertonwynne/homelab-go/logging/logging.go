// Package logging installs the process-wide slog handler: JSON on stdout,
// with the active span's trace and span ids on every record logged with a
// context. Alloy tails stdout into Loki, and trace_id is what links a log
// line to its trace in Tempo.
//
// Never log user identifiers or user content through this — no email, session
// or CSRF value, Access JWT, request header or body. These lines leave the
// cluster and sit in Loki with its retention.
package logging

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// Init must run before anything that logs; until it does, slog writes
// unstructured text to stderr.
func Init() {
	slog.SetDefault(slog.New(&traceHandler{
		Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	}))
}

type traceHandler struct {
	slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	// An absent or unsampled span yields all-zero ids, which would look like a
	// trace id and invite a Tempo lookup that can only come back empty.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup have to rewrap. The embedded handler's versions
// return the bare JSON handler, so slog.With would silently drop the ids.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
