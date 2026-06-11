package ports

import (
	"context"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/google/uuid"

	auctionevents "molot/internal/auction/events"
	participantevents "molot/internal/participant/events"
)

// Consumer-side projection interfaces — implemented by adapters, wired
// in service/. Every handler is idempotent (at-least-once, rule 37):
// upserts, monotonic guards and deletes; idempotency keys are in
// ARCHITECTURE §6.6.

type BidderProfileProjection interface {
	UpsertRegistered(ctx context.Context, bidderID uuid.UUID, displayName string) error
	MarkVerified(ctx context.Context, bidderID uuid.UUID) error
}

type CatalogProjection interface {
	UpsertListed(ctx context.Context, auctionID uuid.UUID, title string, sellerID uuid.UUID,
		currency string, startPriceMinor int64, endsAt time.Time) error
	ApplyBid(ctx context.Context, auctionID uuid.UUID, amountMinor int64, bidCount int, newEndsAt time.Time) error
	Remove(ctx context.Context, auctionID uuid.UUID) error
}

type DashboardProjection interface {
	UpsertListed(ctx context.Context, auctionID, sellerID uuid.UUID, title, currency string, endsAt time.Time) error
	ApplyBid(ctx context.Context, auctionID uuid.UUID, bidCount int, newEndsAt time.Time) error
	ApplyClosed(ctx context.Context, auctionID uuid.UUID, outcome string, hammerMinor int64) error
	ApplyCancelled(ctx context.Context, auctionID uuid.UUID) error
	ApplySettlement(ctx context.Context, auctionID uuid.UUID, settlementStatus string, outcome string) error
	MarkRelisted(ctx context.Context, originalID uuid.UUID) error
}

// RegisterEventHandlers subscribes the auction context's handlers:
// participant-events feed the bidder_profiles projection; the
// context's OWN auction-events feed the catalog and dashboard read
// projections (§5). Handler names double as consumer groups.
func RegisterEventHandlers(
	processor *cqrs.EventProcessor,
	profiles BidderProfileProjection,
	catalog CatalogProjection,
	dashboard DashboardProjection,
) error {
	if processor == nil {
		panic("RegisterEventHandlers: nil processor")
	}
	if profiles == nil || catalog == nil || dashboard == nil {
		panic("RegisterEventHandlers: nil projection")
	}

	return processor.AddHandlers(
		cqrs.NewEventHandler("auction.OnParticipantRegistered",
			func(ctx context.Context, e *participantevents.ParticipantRegisteredV1) error {
				id, err := parseEventUUID("participant_id", e.ParticipantID)
				if err != nil {
					return err
				}
				return profiles.UpsertRegistered(ctx, id, e.DisplayName)
			}),

		cqrs.NewEventHandler("auction.OnParticipantVerified",
			func(ctx context.Context, e *participantevents.ParticipantVerifiedV1) error {
				id, err := parseEventUUID("participant_id", e.ParticipantID)
				if err != nil {
					return err
				}
				return profiles.MarkVerified(ctx, id)
			}),

		cqrs.NewEventHandler("auction.OnAuctionListed",
			func(ctx context.Context, e *auctionevents.AuctionListedV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				sellerID, err := parseEventUUID("seller_id", e.SellerID)
				if err != nil {
					return err
				}
				if err := catalog.UpsertListed(ctx, auctionID, e.Title, sellerID,
					e.Currency, e.StartPriceMinor, e.EndsAt); err != nil {
					return err
				}
				return dashboard.UpsertListed(ctx, auctionID, sellerID, e.Title, e.Currency, e.EndsAt)
			}),

		cqrs.NewEventHandler("auction.OnBidPlaced",
			func(ctx context.Context, e *auctionevents.BidPlacedV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				if err := catalog.ApplyBid(ctx, auctionID, e.AmountMinor, e.BidCount, e.NewEndsAt); err != nil {
					return err
				}
				return dashboard.ApplyBid(ctx, auctionID, e.BidCount, e.NewEndsAt)
			}),

		cqrs.NewEventHandler("auction.OnAuctionClosed",
			func(ctx context.Context, e *auctionevents.AuctionClosedV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				if err := catalog.Remove(ctx, auctionID); err != nil {
					return err
				}
				return dashboard.ApplyClosed(ctx, auctionID, e.Outcome, e.HammerPriceMinor)
			}),

		cqrs.NewEventHandler("auction.OnAuctionCancelled",
			func(ctx context.Context, e *auctionevents.AuctionCancelledV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				if err := catalog.Remove(ctx, auctionID); err != nil {
					return err
				}
				return dashboard.ApplyCancelled(ctx, auctionID)
			}),

		cqrs.NewEventHandler("auction.OnAuctionRelisted",
			func(ctx context.Context, e *auctionevents.AuctionRelistedV1) error {
				originalID, err := parseEventUUID("original_auction_id", e.OriginalAuctionID)
				if err != nil {
					return err
				}
				// The replacement creates its own rows via its
				// AuctionListedV1; here we stamp the original.
				return dashboard.MarkRelisted(ctx, originalID)
			}),

		cqrs.NewEventHandler("auction.OnSaleSettled",
			func(ctx context.Context, e *auctionevents.SaleSettledV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				return dashboard.ApplySettlement(ctx, auctionID, "settled", "")
			}),

		cqrs.NewEventHandler("auction.OnSaleFailed",
			func(ctx context.Context, e *auctionevents.SaleFailedV1) error {
				auctionID, err := parseEventUUID("auction_id", e.AuctionID)
				if err != nil {
					return err
				}
				// FailSale flips the aggregate outcome to not_sold;
				// mirror it on the dashboard.
				return dashboard.ApplySettlement(ctx, auctionID, "failed", "not_sold")
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
