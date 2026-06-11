package adapters

import (
	"context"
	"database/sql"
	"fmt"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/google/uuid"

	"molot/internal/billing/domain/invoice"
	billingevents "molot/internal/billing/events"
	cwatermill "molot/internal/common/watermill"
)

// publishDomainEvents maps drained domain events to integration V1
// events and publishes them through the transactional outbox bound to
// tx — atomically with the aggregate persist (§4.1, BOOK_AUDIT rule 35).
func publishDomainEvents(ctx context.Context, tx *sql.Tx, wmLogger wm.LoggerAdapter, events []invoice.DomainEvent) error {
	integration := make([]any, 0, len(events))
	for _, e := range events {
		if ie, ok := toIntegrationEvent(e); ok {
			integration = append(integration, ie)
		}
	}
	if len(integration) == 0 {
		return nil
	}

	publisher, err := cwatermill.NewTxPublisher(tx, wmLogger)
	if err != nil {
		return fmt.Errorf("create outbox publisher: %w", err)
	}
	bus, err := cwatermill.NewEventBus(
		publisher,
		func(string) string { return billingevents.Topic },
		wmLogger,
	)
	if err != nil {
		return fmt.Errorf("create event bus: %w", err)
	}
	for _, ie := range integration {
		if err := bus.Publish(ctx, ie); err != nil {
			return fmt.Errorf("publish billing event to outbox: %w", err)
		}
	}
	return nil
}

// toIntegrationEvent maps one rich domain event to its flat V1 shape.
// InvoiceVoided intentionally maps to nothing: void is invoked
// synchronously by the saga and has no other consumers (§2.2).
func toIntegrationEvent(e invoice.DomainEvent) (any, bool) {
	switch e := e.(type) {
	case invoice.InvoiceIssued:
		return &billingevents.InvoiceIssuedV1{
			EventID:         uuid.NewString(),
			InvoiceID:       e.InvoiceID.String(),
			AuctionID:       e.AuctionID.String(),
			DebtorID:        e.Debtor.String(),
			HammerMinor:     e.Hammer.Amount(),
			CommissionMinor: e.Commission.Amount(),
			TotalMinor:      e.Total.Amount(),
			Currency:        e.Total.Currency().String(),
			DueAt:           e.DueAt,
			Attempt:         e.Attempt.Int(),
			OccurredAt:      e.OccurredAt,
		}, true
	case invoice.InvoicePaid:
		return &billingevents.InvoicePaidV1{
			EventID:    uuid.NewString(),
			InvoiceID:  e.InvoiceID.String(),
			AuctionID:  e.AuctionID.String(),
			DebtorID:   e.Debtor.String(),
			TotalMinor: e.Total.Amount(),
			Currency:   e.Total.Currency().String(),
			OccurredAt: e.OccurredAt,
		}, true
	case invoice.InvoiceExpired:
		return &billingevents.InvoiceExpiredV1{
			EventID:    uuid.NewString(),
			InvoiceID:  e.InvoiceID.String(),
			AuctionID:  e.AuctionID.String(),
			DebtorID:   e.Debtor.String(),
			Attempt:    e.Attempt.Int(),
			OccurredAt: e.OccurredAt,
		}, true
	case invoice.InvoiceVoided:
		return nil, false
	default:
		// A new domain event must be mapped (or skipped) consciously.
		panic(fmt.Sprintf("billing: unmapped domain event %T", e))
	}
}
