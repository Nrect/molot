package watermill_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	cwatermill "molot/internal/common/watermill"
)

type recordingPublisher struct {
	topics   []string
	messages []*message.Message
	err      error
	closed   bool
}

func (p *recordingPublisher) Publish(topic string, messages ...*message.Message) error {
	p.topics = append(p.topics, topic)
	p.messages = append(p.messages, messages...)
	return p.err
}

func (p *recordingPublisher) Close() error {
	p.closed = true
	return nil
}

func TestTracingPublisherDecoratorInjectsTraceContext(t *testing.T) {
	// The decorator uses the global tracer provider and propagator (set
	// in production by common/tracing); the test installs its own and
	// restores them (no t.Parallel — global OTel state).
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	t.Cleanup(setGlobalPropagator(propagation.TraceContext{}))
	t.Cleanup(setGlobalTracerProvider(provider))

	ctx, parent := provider.Tracer("test").Start(context.Background(), "commands/PlaceBid")
	defer parent.End()

	msg := message.NewMessage(wm.NewUUID(), []byte(`{}`))
	msg.SetContext(ctx)

	inner := &recordingPublisher{}
	pub := cwatermill.NewTracingPublisherDecorator(inner)

	require.NoError(t, pub.Publish("auction-events", msg))

	// The message reached the wrapped publisher with traceparent
	// metadata carrying the parent trace id.
	require.Len(t, inner.messages, 1)
	traceparent := inner.messages[0].Metadata.Get("traceparent")
	require.NotEmpty(t, traceparent, "W3C trace context must be injected into metadata")
	assert.Contains(t, traceparent, parent.SpanContext().TraceID().String())

	// A producer span "publish <topic>" was recorded under the same trace.
	var publishSpans []tracetest.SpanStub
	for _, span := range exporter.GetSpans() {
		if span.Name == "publish auction-events" {
			publishSpans = append(publishSpans, span)
		}
	}
	require.Len(t, publishSpans, 1)
	assert.Equal(t, parent.SpanContext().TraceID(), publishSpans[0].SpanContext.TraceID())

	require.NoError(t, pub.Close())
	assert.True(t, inner.closed)
}

func TestTracingPublisherDecoratorPropagatesPublishError(t *testing.T) {
	pubErr := errors.New("outbox insert failed")
	inner := &recordingPublisher{err: pubErr}

	pub := cwatermill.NewTracingPublisherDecorator(inner)
	msg := message.NewMessage(wm.NewUUID(), nil)

	assert.ErrorIs(t, pub.Publish("billing-events", msg), pubErr)
}

func TestNewRouter(t *testing.T) {
	router, err := cwatermill.NewRouter(
		cwatermill.NewLogger(slog.New(slog.DiscardHandler)),
		&recordingPublisher{},
		metricnoop.NewMeterProvider(),
	)

	require.NoError(t, err)
	require.NotNil(t, router)
	assert.NoError(t, router.Close())
}

// setGlobalPropagator swaps the global OTel propagator and returns the
// restore func for t.Cleanup.
func setGlobalPropagator(p propagation.TextMapPropagator) func() {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(p)
	return func() { otel.SetTextMapPropagator(previous) }
}

// setGlobalTracerProvider swaps the global OTel tracer provider and
// returns the restore func for t.Cleanup.
func setGlobalTracerProvider(tp trace.TracerProvider) func() {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return func() { otel.SetTracerProvider(previous) }
}
