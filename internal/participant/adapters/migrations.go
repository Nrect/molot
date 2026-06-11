package adapters

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"

	cwatermill "molot/internal/common/watermill"
	"molot/internal/participant/events"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations exposes the context's goose migrations rooted at the .sql
// files (goose providers read the FS root), ready to be appended to the
// monolith's migration list in main.
var Migrations fs.FS = mustSubMigrations()

func mustSubMigrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(fmt.Sprintf("participant migrations embed: %v", err))
	}
	return sub
}

// InitializeEventsSchema creates the watermill tables for the
// participant-events topic up front. The repositories publish through
// NewTxPublisher, which deliberately never auto-creates schema (an
// implicit commit inside the business transaction); without a consumer
// of this topic in the process, the first publish would otherwise hit a
// missing table.
func InitializeEventsSchema(db *sql.DB, logger wm.LoggerAdapter) error {
	subscriber, err := cwatermill.NewSQLSubscriber(db, "participant-events-schema-init", time.Second, logger)
	if err != nil {
		return fmt.Errorf("create schema-init subscriber: %w", err)
	}
	defer func() { _ = subscriber.Close() }()

	initializer, ok := subscriber.(interface{ SubscribeInitialize(topic string) error })
	if !ok {
		return errors.New("sql subscriber does not support SubscribeInitialize")
	}
	if err := initializer.SubscribeInitialize(events.Topic); err != nil {
		return fmt.Errorf("initialize %s schema: %w", events.Topic, err)
	}
	return nil
}
