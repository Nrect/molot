// Package service is the composition root of the notification context:
// it wires the fake email sender and the Postgres stores into the event
// handlers and registers them on the shared watermill router. The
// context has no HTTP API and no facade — it is a pure event consumer.
package service

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"

	cwatermill "molot/internal/common/watermill"
	"molot/internal/notification/adapters"
	"molot/internal/notification/ports"
)

// Migrations is the context's goose migration set (notification
// schema, ARCHITECTURE.md §7), rooted at the .sql files — ready for
// goose.NewProvider with the per-context version table
// goose_db_version_notification, same shape as every other context's
// service.Migrations.
var Migrations fs.FS = mustSubFS()

func mustSubFS() fs.FS {
	sub, err := fs.Sub(adapters.Migrations, "migrations")
	if err != nil {
		panic("notification service: migrations FS: " + err.Error())
	}
	return sub
}

// Service is the runtime assembly of the notification context.
type Service struct {
	handlers ports.EventHandlers
	wmLogger wm.LoggerAdapter
}

// NewService wires the production adapters. It panics on a nil
// dependency: composition-time failure beats a broken consumer.
func NewService(db *sql.DB, logger *slog.Logger) *Service {
	if db == nil {
		panic("notification.NewService: nil db")
	}
	if logger == nil {
		panic("notification.NewService: nil logger")
	}

	handlers := ports.NewEventHandlers(
		adapters.NewFakeEmailSender(logger),
		adapters.NewRecipientsPG(db),
		adapters.NewSellersPG(db),
		adapters.NewSentLogPG(db),
	)
	return &Service{
		handlers: handlers,
		wmLogger: cwatermill.NewLogger(logger),
	}
}

// RegisterEventHandlers subscribes the context's §4.3 handlers on the
// shared router. subscriberConstructor receives the handler name and
// must return a subscriber whose consumer group is that name, so every
// handler keeps an independent offset on its topic.
func (s *Service) RegisterEventHandlers(
	router *message.Router,
	subscriberConstructor func(handlerName string) (message.Subscriber, error),
) error {
	// Subscriptions sit on multi-event topics (auction-events carries 8
	// event types, we consume 6); the common helper acks foreign event
	// types (AckOnUnknownEvent) instead of erroring them into retries
	// and the dead letter.
	processor, err := cwatermill.NewEventProcessor(router, ports.SubscribeTopicFor, subscriberConstructor, s.wmLogger)
	if err != nil {
		return fmt.Errorf("notification: create event processor: %w", err)
	}

	if err := processor.AddHandlers(s.handlers.Handlers()...); err != nil {
		return fmt.Errorf("notification: register event handlers: %w", err)
	}
	return nil
}
