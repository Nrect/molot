// cqrs event bus / processor wiring. Events are marshalled as JSON
// with the bare struct name ("AuctionClosedV1") as the event name, so
// payloads stay readable in the database and the dead-letter runbook.
package watermill

import (
	"fmt"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// Marshaler is the single event (un)marshaler of the platform; the bus
// and every processor must share it so event names match.
var Marshaler = cqrs.JSONMarshaler{GenerateName: cqrs.StructName}

// NewEventBus builds the publishing side of the event bus.
// generateTopic maps an event name ("AuctionClosedV1") to its
// integration topic (the events/ packages export Topic constants);
// publisher is typically a NewTxPublisher bound to the repository
// transaction (outbox).
func NewEventBus(
	publisher message.Publisher,
	generateTopic func(eventName string) string,
	logger wm.LoggerAdapter,
) (*cqrs.EventBus, error) {
	bus, err := cqrs.NewEventBusWithConfig(publisher, cqrs.EventBusConfig{
		GeneratePublishTopic: func(params cqrs.GenerateEventPublishTopicParams) (string, error) {
			return generateTopic(params.EventName), nil
		},
		Marshaler: Marshaler,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create event bus: %w", err)
	}
	return bus, nil
}

// NewEventProcessor builds the consuming side on the shared router.
// generateTopic maps an event name to the topic it is consumed from;
// subscriberConstructor receives the handler name and should return a
// NewSQLSubscriber with the handler name as consumer group, giving each
// handler an independent offset (idempotency lives in the handlers,
// §6.6). Topics carry multiple event types while a handler consumes
// one, so messages with an unknown event name on the handler's topic
// are acked and ignored (AckOnUnknownEvent) instead of erroring through
// the retry middleware into the dead letter.
func NewEventProcessor(
	router *message.Router,
	generateTopic func(eventName string) string,
	subscriberConstructor func(handlerName string) (message.Subscriber, error),
	logger wm.LoggerAdapter,
) (*cqrs.EventProcessor, error) {
	processor, err := cqrs.NewEventProcessorWithConfig(router, cqrs.EventProcessorConfig{
		AckOnUnknownEvent: true,
		GenerateSubscribeTopic: func(params cqrs.EventProcessorGenerateSubscribeTopicParams) (string, error) {
			return generateTopic(params.EventName), nil
		},
		SubscriberConstructor: func(params cqrs.EventProcessorSubscriberConstructorParams) (message.Subscriber, error) {
			return subscriberConstructor(params.HandlerName)
		},
		Marshaler: Marshaler,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create event processor: %w", err)
	}
	return processor, nil
}
