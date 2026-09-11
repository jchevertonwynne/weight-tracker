package db

import (
	"context"

	"github.com/jchevertonwynne/homelab-go/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// tracer is named after this package's import path, the OpenTelemetry
// convention for library-level instrumentation — it's how a span's origin
// is distinguishable from the app's own HTTP-level tracer (see
// internal/tracing) without this package needing to know the service name.
var tracer = otel.Tracer("weight-tracker/internal/db")

// withSpan is tracing.Op with this package's span naming ("db.<op>") and
// db.system attribute fixed, so call sites don't repeat either. Every
// exported, request-path function in this package wraps its body this
// way, so a slow query shows up as its own span in a trace rather than
// being folded into whatever HTTP handler called it. Startup-only
// functions (Open, migrations, JournalMode) don't — nothing is tracing
// them yet when they run.
func withSpan[T any](ctx context.Context, op string, fn func(ctx context.Context) (T, error)) (T, error) {
	return tracing.Op(ctx, tracer, "db."+op, fn, attribute.String("db.system", "sqlite"))
}

// withSpanErr is withSpan for the functions in this package that return
// only an error.
func withSpanErr(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	return tracing.Do(ctx, tracer, "db."+op, fn, attribute.String("db.system", "sqlite"))
}
