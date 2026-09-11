package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// The trace ids are the only reason this package exists rather than a bare
// slog.NewJSONHandler in main, so both halves of the rule are worth pinning:
// attach them when there is a real span, and attach nothing when there isn't.

func TestHandleAttachesTraceIDsFromSpanContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(&traceHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	logger.ErrorContext(ctx, "write index response", "error", "broken pipe")

	got := decode(t, buf.Bytes())
	if got["trace_id"] != traceID.String() {
		t.Errorf("trace_id = %v, want %q", got["trace_id"], traceID.String())
	}
	if got["span_id"] != spanID.String() {
		t.Errorf("span_id = %v, want %q", got["span_id"], spanID.String())
	}
	// Grafana's derived field matches on the level and the message too, so a
	// record that loses them on the way through the wrapper is no more
	// useful than one with no ids at all.
	if got["level"] != "ERROR" {
		t.Errorf("level = %v, want %q", got["level"], "ERROR")
	}
	if got["msg"] != "write index response" {
		t.Errorf("msg = %v, want %q", got["msg"], "write index response")
	}
}

func TestHandleOmitsTraceIDsWithoutASpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(&traceHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	// Startup and shutdown records look like this, as does every record in
	// the cluster before Alloy's receiver is reachable. An all-zero trace id
	// here would look like a real one in Loki and link to nothing in Tempo.
	logger.Info("listening", "addr", ":8090")

	got := decode(t, buf.Bytes())
	if _, ok := got["trace_id"]; ok {
		t.Errorf("trace_id present without a span: %v", got["trace_id"])
	}
	if _, ok := got["span_id"]; ok {
		t.Errorf("span_id present without a span: %v", got["span_id"])
	}
}

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("log line is not JSON (%v): %s", err, line)
	}
	return got
}
