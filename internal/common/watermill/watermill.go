// Package watermill owns the message-bus infrastructure of the
// monolith (ARCHITECTURE.md §4.4): the router with its strict
// middleware order (CorrelationID → PoisonQueue → Retry → Recoverer →
// observe), Postgres-backed pub/sub for the transactional outbox, the
// cqrs event bus/processor, and W3C trace propagation through message
// metadata.
package watermill

import (
	"fmt"
	"log/slog"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"

	"molot/internal/common/logs"
)

// DeadLetterTopic is where the PoisonQueue middleware parks messages
// that exhausted their retries (§6.8: alert on size > 0, redeliver via
// runbook).
const DeadLetterTopic = "events.dead_letter"

const tracerName = "molot/internal/common/watermill"

// NewLogger adapts the process slog logger to watermill's logging
// interface.
func NewLogger(logger *slog.Logger) wm.LoggerAdapter {
	return wm.NewSlogLogger(logger)
}

// NewRouter builds the single message router of the binary with the
// middleware chain in this strict order:
//
//	CorrelationID → PoisonQueue(deadLetterPublisher, "events.dead_letter")
//	→ Retry{5 attempts, exponential backoff capped at 30s} → retryCounter
//	→ Recoverer → observe
//
// Recoverer sits innermost so panics become errors that Retry retries
// and PoisonQueue ultimately parks. retryCounter sits between Retry and
// Recoverer: each handler error visible at that layer is exactly one
// retry trigger, and it increments molot_bus_retries_total{handler}.
// The observe middleware (innermost of all) starts an
// "events/<HandlerName>" span from the trace context propagated in
// message metadata and puts the correlation id into the handler context
// for log enrichment.
func NewRouter(logger wm.LoggerAdapter, deadLetterPublisher message.Publisher, meterProvider metric.MeterProvider) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return nil, fmt.Errorf("create watermill router: %w", err)
	}

	poisonQueue, err := middleware.PoisonQueue(deadLetterPublisher, DeadLetterTopic)
	if err != nil {
		return nil, fmt.Errorf("create poison queue middleware: %w", err)
	}

	retriesTotal, err := meterProvider.Meter("molot/internal/common/watermill").
		Int64Counter("molot_bus_retries_total",
			metric.WithDescription("Number of message handler errors that triggered a retry attempt, per handler"))
	if err != nil {
		return nil, fmt.Errorf("create retries counter: %w", err)
	}

	retry := middleware.Retry{
		MaxRetries:      5,
		InitialInterval: 100 * time.Millisecond,
		Multiplier:      2,
		MaxInterval:     30 * time.Second,
		Logger:          logger,
	}

	retryCounter := func(h message.HandlerFunc) message.HandlerFunc {
		return func(msg *message.Message) ([]*message.Message, error) {
			produced, err := h(msg)
			if err != nil {
				handlerName := message.HandlerNameFromCtx(msg.Context())
				if handlerName == "" {
					handlerName = "unknown"
				}
				retriesTotal.Add(msg.Context(), 1, metric.WithAttributes(attribute.String("handler", handlerName)))
			}
			return produced, err
		}
	}

	router.AddMiddleware(
		middleware.CorrelationID,
		poisonQueue,
		retry.Middleware,
		retryCounter,
		middleware.Recoverer,
		observe,
	)

	return router, nil
}

// observe enriches every handler invocation (including each retry
// attempt) with a consumer span and the correlation id:
//
//   - extracts the W3C trace context injected into message metadata by
//     the publisher decorator and starts span "events/<HandlerName>"
//     parented to the publishing span;
//   - copies the watermill correlation id into the context, so every
//     log record of the handler carries correlation_id (common/logs).
func observe(h message.HandlerFunc) message.HandlerFunc {
	return func(msg *message.Message) ([]*message.Message, error) {
		ctx := msg.Context()

		if correlationID := middleware.MessageCorrelationID(msg); correlationID != "" {
			ctx = logs.ContextWithCorrelationID(ctx, correlationID)
		}

		ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(msg.Metadata))

		handlerName := message.HandlerNameFromCtx(ctx)
		if handlerName == "" {
			handlerName = "unknown"
		}

		ctx, span := otel.Tracer(tracerName).Start(ctx, "events/"+handlerName)
		defer span.End()

		msg.SetContext(ctx)

		produced, err := h(msg)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return produced, err
	}
}

// metadataCarrier adapts message metadata to the OTel propagation
// carrier, so traceparent/baggage travel inside the message.
type metadataCarrier message.Metadata

var _ propagation.TextMapCarrier = metadataCarrier{}

func (c metadataCarrier) Get(key string) string { return message.Metadata(c).Get(key) }

func (c metadataCarrier) Set(key, value string) { message.Metadata(c).Set(key, value) }

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
