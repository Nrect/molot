package logs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"molot/internal/common/logs"
)

func TestContextHandlerAddsCorrelationID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(logs.NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := logs.ContextWithCorrelationID(context.Background(), "corr-123")
	logger.InfoContext(ctx, "command handled", "handler", "PlaceBid")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if got := record["correlation_id"]; got != "corr-123" {
		t.Errorf("correlation_id = %v, want corr-123", got)
	}
	if got := record["handler"]; got != "PlaceBid" {
		t.Errorf("handler attr lost: %v", got)
	}
}

func TestContextHandlerWithoutCorrelationID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(logs.NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "no correlation")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if _, ok := record["correlation_id"]; ok {
		t.Error("correlation_id must be absent when the context carries none")
	}
}

func TestContextHandlerAddsTraceAndSpanIDs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(logs.NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:  trace.SpanID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x01, 0x02},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	logger.InfoContext(ctx, "inside a span")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if got := record["trace_id"]; got != spanCtx.TraceID().String() {
		t.Errorf("trace_id = %v, want %v", got, spanCtx.TraceID())
	}
	if got := record["span_id"]; got != spanCtx.SpanID().String() {
		t.Errorf("span_id = %v, want %v", got, spanCtx.SpanID())
	}
}

func TestContextHandlerWithoutSpan(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(logs.NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "no span")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if _, ok := record["trace_id"]; ok {
		t.Error("trace_id must be absent when the context carries no span")
	}
}

func TestCorrelationIDRoundTrip(t *testing.T) {
	t.Parallel()

	if _, ok := logs.CorrelationIDFromContext(context.Background()); ok {
		t.Error("empty context must report no correlation id")
	}

	ctx := logs.ContextWithCorrelationID(context.Background(), "abc")
	id, ok := logs.CorrelationIDFromContext(ctx)
	if !ok || id != "abc" {
		t.Errorf("round trip = (%q, %v), want (abc, true)", id, ok)
	}
}
