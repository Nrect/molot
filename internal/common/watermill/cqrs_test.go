package watermill_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"

	cwatermill "molot/internal/common/watermill"
)

// Two event types sharing one topic, mirroring production multi-event
// topics (auction-events carries 8 types while each handler consumes
// one). The processor must ack the type the handler did not subscribe
// to instead of erroring it through Retry into the dead letter.
type orderPlacedV1 struct {
	OrderID string `json:"order_id"`
}

type orderCancelledV1 struct {
	OrderID string `json:"order_id"`
}

const testEventsTopic = "test-events"

// concurrentRecordingPublisher records publishes under a mutex: the
// PoisonQueue middleware would publish from the router goroutine.
type concurrentRecordingPublisher struct {
	mu       sync.Mutex
	messages []*message.Message
}

func (p *concurrentRecordingPublisher) Publish(_ string, messages ...*message.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, messages...)
	return nil
}

func (p *concurrentRecordingPublisher) Close() error { return nil }

func (p *concurrentRecordingPublisher) Messages() []*message.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*message.Message(nil), p.messages...)
}

func TestNewEventProcessorAcksUnknownEventTypesOnTopic(t *testing.T) {
	logger := cwatermill.NewLogger(slog.New(slog.DiscardHandler))

	deadLetter := &concurrentRecordingPublisher{}
	router, err := cwatermill.NewRouter(logger, deadLetter, metricnoop.NewMeterProvider())
	require.NoError(t, err)

	// BlockPublishUntilSubscriberAck makes Publish return only once the
	// handler chain acked the message, so the assertions below run after
	// the processor decided the message's fate.
	pubSub := gochannel.NewGoChannel(gochannel.Config{
		BlockPublishUntilSubscriberAck: true,
	}, logger)

	processor, err := cwatermill.NewEventProcessor(
		router,
		func(string) string { return testEventsTopic },
		func(string) (message.Subscriber, error) { return pubSub, nil },
		logger,
	)
	require.NoError(t, err)

	var (
		mu      sync.Mutex
		handled []string
	)
	require.NoError(t, processor.AddHandlers(
		cqrs.NewEventHandler("OnOrderPlaced", func(_ context.Context, e *orderPlacedV1) error {
			mu.Lock()
			defer mu.Unlock()
			handled = append(handled, e.OrderID)
			return nil
		}),
	))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	routerDone := make(chan error, 1)
	go func() { routerDone <- router.Run(ctx) }()
	select {
	case <-router.Running():
	case <-ctx.Done():
		t.Fatal("router did not start")
	}

	// The foreign event type first: it must be acked and ignored. With
	// AckOnUnknownEvent unset this would burn 5 retries and land in the
	// dead letter instead.
	publishEvent(t, pubSub, &orderCancelledV1{OrderID: "cancelled-1"})
	// The subscribed event type still reaches the handler.
	publishEvent(t, pubSub, &orderPlacedV1{OrderID: "placed-1"})

	require.NoError(t, router.Close())
	require.NoError(t, <-routerDone)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"placed-1"}, handled,
		"only the subscribed event type reaches the handler")
	assert.Empty(t, deadLetter.Messages(),
		"a foreign event type on the topic must be acked, not dead-lettered")
}

// publishEvent marshals the event exactly as the production event bus
// would (shared Marshaler, struct name as event name) and publishes it.
func publishEvent(t *testing.T, pub message.Publisher, event any) {
	t.Helper()
	msg, err := cwatermill.Marshaler.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, pub.Publish(testEventsTopic, msg))
}
