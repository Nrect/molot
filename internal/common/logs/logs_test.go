package logs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

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
