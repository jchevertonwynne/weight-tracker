package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// record logs through the trace handler and decodes the JSON Alloy would ship.
func record(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	h := &traceHandler{Handler: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})}
	slog.New(h).ErrorContext(ctx, "encode response", "error", "broken pipe")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	return got
}

func TestAttachesTraceIDsFromContext(t *testing.T) {
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))

	got := record(t, ctx)
	if got["trace_id"] != traceID.String() {
		t.Errorf("trace_id = %v, want %s", got["trace_id"], traceID)
	}
	if got["span_id"] != spanID.String() {
		t.Errorf("span_id = %v, want %s", got["span_id"], spanID)
	}
	// The level and static message are what a Loki query selects on.
	if got["msg"] != "encode response" {
		t.Errorf("msg = %v, want a static message", got["msg"])
	}
	if got["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", got["level"])
	}
}

// Startup and shutdown lines have no span; all-zero ids would be a dead link.
func TestOmitsTraceIDsWithoutASpan(t *testing.T) {
	got := record(t, context.Background())
	if _, ok := got["trace_id"]; ok {
		t.Errorf("trace_id present without a span: %v", got["trace_id"])
	}
	if _, ok := got["span_id"]; ok {
		t.Errorf("span_id present without a span: %v", got["span_id"])
	}
}

// slog.With goes through WithAttrs, which must rewrap or the ids are lost.
func TestDerivedLoggersKeepTraceIDs(t *testing.T) {
	traceID := trace.TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	}))

	var buf bytes.Buffer
	h := &traceHandler{Handler: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})}
	slog.New(h).With("component", "web").InfoContext(ctx, "listening")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	if got["trace_id"] != traceID.String() {
		t.Errorf("trace_id = %v, want %s", got["trace_id"], traceID)
	}
	if got["component"] != "web" {
		t.Errorf("component = %v, want web", got["component"])
	}
}
