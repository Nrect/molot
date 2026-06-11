package ports

import (
	"context"
	"fmt"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/google/uuid"

	auctionevents "molot/internal/auction/events"
	billingevents "molot/internal/billing/events"
	"molot/internal/settlement/app"
)

// RegisterEventHandlers subscribes the saga's typed handlers (§4.3):
// auction-events drive the start, the award and the fast-forward seams;
// billing-events drive the payment and the timeout forks. Handler names
// double as consumer groups; idempotency keys are in §6.6.
func RegisterEventHandlers(processor *cqrs.EventProcessor, handlers app.EventHandlers) error {
	if processor == nil {
		panic("RegisterEventHandlers: nil processor")
	}

	return processor.AddHandlers(
		cqrs.NewEventHandler("settlement.OnAuctionClosedStartSettlement",
			func(ctx context.Context, e *auctionevents.AuctionClosedV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				// Winner and runner-up are absent on not_sold closings —
				// parse leniently here, validate in the app/domain.
				winnerID, err := parseOptionalEventUUID("winner_id", e.WinnerID)
				if err != nil {
					return err
				}
				runnerUpID, err := parseOptionalEventUUID("runner_up_bidder_id", e.RunnerUpBidderID)
				if err != nil {
					return err
				}
				return handlers.OnAuctionClosed(ctx, app.ClosedAuction{
					AuctionID:         auctionID,
					Outcome:           e.Outcome,
					WinnerID:          winnerID,
					HammerMinor:       e.HammerPriceMinor,
					Currency:          e.Currency,
					RunnerUpID:        runnerUpID,
					RunnerUpMinor:     e.RunnerUpAmountMinor,
					RunnerUpQualifies: e.RunnerUpQualifies,
					RelistGeneration:  e.RelistGeneration,
				})
			}),

		cqrs.NewEventHandler("settlement.OnInvoicePaid",
			func(ctx context.Context, e *billingevents.InvoicePaidV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				invoiceID, err := parseEventUUID("invoice_id", e.InvoiceID)
				if err != nil {
					return err
				}
				return handlers.OnInvoicePaid(ctx, auctionID, invoiceID)
			}),

		cqrs.NewEventHandler("settlement.OnInvoiceExpired",
			func(ctx context.Context, e *billingevents.InvoiceExpiredV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				invoiceID, err := parseEventUUID("invoice_id", e.InvoiceID)
				if err != nil {
					return err
				}
				return handlers.OnInvoiceExpired(ctx, auctionID, invoiceID)
			}),

		cqrs.NewEventHandler("settlement.OnWinnerReassigned",
			func(ctx context.Context, e *auctionevents.WinnerReassignedV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				newWinnerID, err := parseEventUUID("new_winner_id", e.NewWinnerID)
				if err != nil {
					return err
				}
				return handlers.OnWinnerReassigned(ctx, auctionID, newWinnerID, e.PriceMinor, e.Currency)
			}),
	)
}

// parseEventUUID guards against malformed payloads: the error is a
// poison message — Watermill retries then dead-letters it (§6.8 (а)).
func parseEventUUID(field, raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("malformed %s %q in event: %w", field, raw, err)
	}
	return id, nil
}

// parseOptionalEventUUID treats "" as absent (uuid.Nil).
func parseOptionalEventUUID(field, raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, nil
	}
	return parseEventUUID(field, raw)
}
