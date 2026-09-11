// Package tracing exports spans over OTLP/gRPC to Alloy's in-cluster
// receiver, which forwards them to Grafana Cloud Tempo. Always sampled —
// Alloy is the only hop and the traffic here is nowhere near worth trimming.
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

// Init points the global TracerProvider at endpoint (host:port of Alloy's
// OTLP/gRPC receiver), plaintext because it never leaves the cluster network.
// An empty endpoint is a no-op, leaving the no-op tracer in place for local
// dev.
//
// Defer the returned func with a fresh context — the one passed here may
// already be cancelled by shutdown — or the last batch of spans is dropped.
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
	// W3C traceparent, so a trace arriving with one continues rather than
	// starting a new root.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// Middleware gives every request a server span and records the standard HTTP
// semantic-convention attributes.
func Middleware(serviceName string, h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, serviceName)
}
