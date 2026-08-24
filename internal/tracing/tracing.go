// Package tracing wires up OpenTelemetry distributed tracing: spans are
// exported over OTLP/gRPC to Alloy's in-cluster receiver, which forwards
// them to Grafana Cloud Tempo. There is no local exporter and no sampling
// decision made here beyond "always sample" — Alloy is the only hop, and
// data volume at this app's traffic is nowhere near worth trimming.
package tracing

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Init configures the global TracerProvider to batch spans to endpoint
// (host:port of Alloy's OTLP/gRPC receiver) over a plaintext connection —
// everything between here and Alloy stays inside the cluster network, so
// there is no TLS hop to terminate. Passing an empty endpoint is a no-op:
// the global provider is left as OpenTelemetry's default no-op tracer, so
// the app runs unchanged with no collector reachable, such as in local dev.
//
// The returned func flushes and closes the exporter; callers should defer
// it (with a fresh, short-lived context — the one passed to Init may
// already be cancelled by shutdown) so in-flight spans aren't dropped on
// exit.
func Init(ctx context.Context, serviceName, endpoint string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	// W3C traceparent: lets a trace that arrives with one (e.g. hand-added
	// by Cloudflare in front) continue rather than starting a new root, and
	// is what any future downstream call from this app would propagate.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// Middleware wraps h so every request gets a server span, named after
// serviceName. otelhttp reads/writes the W3C traceparent header and records
// standard HTTP semantic-convention attributes (method, route, status)
// without each handler having to do it by hand.
func Middleware(serviceName string, h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, serviceName)
}
