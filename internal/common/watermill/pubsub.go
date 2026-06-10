// Postgres-backed pub/sub (watermill-sql v4) — the transactional
// outbox of ARCHITECTURE.md §4.4. Publishing happens either over the
// pool (infrastructure topics like the dead letter) or inside an open
// *sql.Tx (NewTxPublisher), so events commit atomically with the state
// change that produced them.
package watermill

import (
	"database/sql"
	"fmt"

	wm "github.com/ThreeDotsLabs/watermill"
	wmsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// NewSQLSubscriber builds a Postgres subscriber for one consumer group.
// Consumer groups track offsets independently, so each event handler
// (group = handler name, see NewEventProcessor) receives every message.
// The first subscription initializes the watermill tables.
func NewSQLSubscriber(db *sql.DB, consumerGroup string, logger wm.LoggerAdapter) (message.Subscriber, error) {
	subscriber, err := wmsql.NewSubscriber(
		wmsql.BeginnerFromStdSQL(db),
		wmsql.SubscriberConfig{
			ConsumerGroup:    consumerGroup,
			SchemaAdapter:    wmsql.DefaultPostgreSQLSchema{},
			OffsetsAdapter:   wmsql.DefaultPostgreSQLOffsetsAdapter{},
			InitializeSchema: true,
		},
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create sql subscriber (group %q): %w", consumerGroup, err)
	}
	return subscriber, nil
}

// NewSQLPublisher builds a Postgres publisher over the pool, wrapped
// with trace propagation. It auto-initializes topic schema on first
// publish — safe here because it never runs inside a caller's
// transaction. Use it for infrastructure publishing (dead letter);
// business events go through NewTxPublisher.
func NewSQLPublisher(db *sql.DB, logger wm.LoggerAdapter) (message.Publisher, error) {
	publisher, err := wmsql.NewPublisher(
		wmsql.BeginnerFromStdSQL(db),
		wmsql.PublisherConfig{
			SchemaAdapter:        wmsql.DefaultPostgreSQLSchema{},
			AutoInitializeSchema: true,
		},
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create sql publisher: %w", err)
	}
	return NewTracingPublisherDecorator(publisher), nil
}

// NewTxPublisher builds a publisher bound to an open *sql.Tx for outbox
// publication: the event INSERT commits or rolls back together with the
// business write. Schema auto-initialization is deliberately off — a
// CREATE TABLE inside the caller's transaction would implicitly commit
// it; topics are initialized by subscribers/pool publishers at startup.
func NewTxPublisher(tx *sql.Tx, logger wm.LoggerAdapter) (message.Publisher, error) {
	publisher, err := wmsql.NewPublisher(
		wmsql.TxFromStdSQL(tx),
		wmsql.PublisherConfig{
			SchemaAdapter:        wmsql.DefaultPostgreSQLSchema{},
			AutoInitializeSchema: false,
		},
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create tx publisher: %w", err)
	}
	return NewTracingPublisherDecorator(publisher), nil
}

// NewTracingPublisherDecorator wraps pub so every published message
// gets a "publish <topic>" producer span and the W3C trace context
// injected into its metadata; the consumer-side observe middleware
// extracts it, stitching the HTTP → command → outbox → handler trace
// (§11).
func NewTracingPublisherDecorator(pub message.Publisher) message.Publisher {
	return tracingPublisher{next: pub}
}

type tracingPublisher struct {
	next message.Publisher
}

func (p tracingPublisher) Publish(topic string, messages ...*message.Message) error {
	tracer := otel.Tracer(tracerName)
	propagator := otel.GetTextMapPropagator()

	spans := make([]trace.Span, 0, len(messages))
	for _, msg := range messages {
		// cqrs buses set the publish context on the message; fall back
		// to a fresh root span when a caller publishes raw messages.
		ctx, span := tracer.Start(msg.Context(), "publish "+topic,
			trace.WithSpanKind(trace.SpanKindProducer))
		propagator.Inject(ctx, metadataCarrier(msg.Metadata))
		spans = append(spans, span)
	}

	err := p.next.Publish(topic, messages...)

	for _, span := range spans {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
	return err
}

func (p tracingPublisher) Close() error {
	return p.next.Close()
}
