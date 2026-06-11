package adapters

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/google/uuid"

	cwatermill "molot/internal/common/watermill"
	"molot/internal/participant/domain/participant"
	"molot/internal/participant/events"
)

// publishIntegrationEvents maps drained domain events to the flat V1
// integration events of internal/participant/events and publishes them
// through the transactional outbox: a publisher bound to the SAME
// *sql.Tx as the aggregate persist (ARCHITECTURE.md §4.1–§4.2), so the
// events commit or roll back together with the state change.
func publishIntegrationEvents(
	ctx context.Context,
	tx *sql.Tx,
	logger wm.LoggerAdapter,
	domainEvents []participant.DomainEvent,
	now time.Time,
) error {
	if len(domainEvents) == 0 {
		return nil
	}

	publisher, err := cwatermill.NewTxPublisher(tx, logger)
	if err != nil {
		return fmt.Errorf("create outbox publisher: %w", err)
	}
	bus, err := cwatermill.NewEventBus(publisher, func(string) string { return events.Topic }, logger)
	if err != nil {
		return fmt.Errorf("create event bus: %w", err)
	}

	for _, domainEvent := range domainEvents {
		integrationEvent, err := mapDomainEvent(domainEvent, now)
		if err != nil {
			return err
		}
		if err := bus.Publish(ctx, integrationEvent); err != nil {
			return fmt.Errorf("publish %T to outbox: %w", integrationEvent, err)
		}
	}
	return nil
}

// mapDomainEvent translates one rich domain event into its flat V1
// integration counterpart. ParticipantRegistered carries no business
// time (Register takes no clock), so it is stamped with the persist
// time; ParticipantVerified keeps its business time.
func mapDomainEvent(e participant.DomainEvent, now time.Time) (any, error) {
	switch e := e.(type) {
	case participant.ParticipantRegistered:
		return events.ParticipantRegisteredV1{
			EventID:       uuid.NewString(),
			ParticipantID: e.ID.String(),
			Email:         e.Email.String(),
			DisplayName:   e.DisplayName,
			OccurredAt:    now.UTC(),
		}, nil
	case participant.ParticipantVerified:
		return events.ParticipantVerifiedV1{
			EventID:       uuid.NewString(),
			ParticipantID: e.ID.String(),
			OccurredAt:    e.OccurredAt.UTC(),
		}, nil
	default:
		return nil, fmt.Errorf("unknown participant domain event %T", e)
	}
}
