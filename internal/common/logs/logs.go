// Package logs configures slog for the monolith: JSON in prod, text
// locally (LOG_FORMAT), with a handler wrapper that enriches every
// record from the request context.
//
// NOTE: trace_id/span_id enrichment from the OTel span context is part
// of this handler's contract but requires the OTel API dependency,
// which is not pinned in go.mod yet. It plugs into contextHandler.Handle
// without changing any public signature.
package logs

import (
	"context"
	"log/slog"
	"os"
)

// NewLogger builds the process logger. format is "json" or "text"
// (anything else falls back to JSON — the production-safe choice).
func NewLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}

	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(os.Stdout, opts)
	default:
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(NewContextHandler(h))
}

// NewContextHandler wraps next with the context-enriching handler used
// by NewLogger; exposed so tests and custom sinks share the exact
// production enrichment path.
func NewContextHandler(next slog.Handler) slog.Handler {
	return contextHandler{next: next}
}

type correlationIDKey struct{}

// ContextWithCorrelationID attaches a message/request correlation id to
// the context; every log record made with this context carries it.
// The watermill consumer side calls this with the CorrelationID
// middleware value before invoking event handlers.
func ContextWithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, correlationID)
}

// CorrelationIDFromContext returns the correlation id, if any.
func CorrelationIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(correlationIDKey{}).(string)
	return id, ok && id != ""
}

// contextHandler decorates a slog.Handler, adding context-scoped
// attributes (correlation_id; trace_id/span_id once OTel is wired).
type contextHandler struct {
	next slog.Handler
}

func (h contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	if id, ok := CorrelationIDFromContext(ctx); ok {
		rec.AddAttrs(slog.String("correlation_id", id))
	}
	return h.next.Handle(ctx, rec)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{next: h.next.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{next: h.next.WithGroup(name)}
}
