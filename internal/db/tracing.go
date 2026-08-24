package db

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is named after this package's import path, the OpenTelemetry
// convention for library-level instrumentation — it's how a span's origin
// is distinguishable from the app's own HTTP-level tracer (see
// internal/tracing) without this package needing to know the service name.
var tracer = otel.Tracer("weight-tracker/internal/db")

// withSpan runs fn inside a child span named "db.<op>", recording fn's
// error onto the span before returning it. Every exported, request-path
// function in this package wraps its body this way, so a slow query shows
// up as its own span in a trace rather than being folded into whatever
// HTTP handler called it. Startup-only functions (Open, migrations,
// JournalMode) don't — nothing is tracing them yet when they run.
func withSpan[T any](ctx context.Context, op string, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx, span := tracer.Start(ctx, "db."+op, trace.WithAttributes(attribute.String("db.system", "sqlite")))
	defer span.End()
	result, err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}

// withSpanErr is withSpan for the functions in this package that return
// only an error.
func withSpanErr(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	_, err := withSpan(ctx, op, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}
