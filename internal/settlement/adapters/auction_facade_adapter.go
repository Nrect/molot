package adapters

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	auctionservice "molot/internal/auction/service"
	"molot/internal/settlement/domain/settlement"
)

// AuctionFacade is the slice of the auction service facade the
// settlement saga consumes (§6, ADR-0004). It is declared here so that
// neither app nor service import the foreign service package — the
// compile-time proof below is the ONLY allowed import of another
// context's service/ (adapters-only, rule 3); the concrete
// auctionservice.Facade value arrives from main through service.Deps.
type AuctionFacade interface {
	AwardToRunnerUp(ctx context.Context, auctionID uuid.UUID) error
	Relist(ctx context.Context, originalID, newID uuid.UUID, startsAt, endsAt time.Time) error
	MarkSaleFailed(ctx context.Context, auctionID uuid.UUID, reason string) error
	ConfirmSettlement(ctx context.Context, auctionID uuid.UUID) error
}

var _ AuctionFacade = auctionservice.Facade{}

// AuctionFacadeAdapter implements the saga's consumer-side auction
// gateways over the synchronous facade, adding the facade/ spans of the
// observability map (§11). Every facade command is idempotent
// ("already in the target state" → nil), which the saga's effect phase
// relies on (§6.1).
type AuctionFacadeAdapter struct {
	facade AuctionFacade
	tracer trace.Tracer
}

func NewAuctionFacadeAdapter(facade AuctionFacade, tracerProvider trace.TracerProvider) AuctionFacadeAdapter {
	if facade == nil {
		panic("NewAuctionFacadeAdapter: nil facade")
	}
	if tracerProvider == nil {
		panic("NewAuctionFacadeAdapter: nil tracer provider")
	}
	return AuctionFacadeAdapter{
		facade: facade,
		tracer: tracerProvider.Tracer("molot/internal/settlement/adapters"),
	}
}

func (a AuctionFacadeAdapter) AwardToRunnerUp(ctx context.Context, auctionID settlement.AuctionID) error {
	return a.traced(ctx, "facade/auction.AwardToRunnerUp", func(ctx context.Context) error {
		return a.facade.AwardToRunnerUp(ctx, auctionID.UUID())
	})
}

func (a AuctionFacadeAdapter) Relist(
	ctx context.Context,
	originalID, newID settlement.AuctionID,
	startsAt, endsAt time.Time,
) error {
	return a.traced(ctx, "facade/auction.Relist", func(ctx context.Context) error {
		return a.facade.Relist(ctx, originalID.UUID(), newID.UUID(), startsAt, endsAt)
	})
}

func (a AuctionFacadeAdapter) MarkSaleFailed(
	ctx context.Context,
	auctionID settlement.AuctionID,
	reason settlement.FailureReason,
) error {
	return a.traced(ctx, "facade/auction.MarkSaleFailed", func(ctx context.Context) error {
		return a.facade.MarkSaleFailed(ctx, auctionID.UUID(), reason.String())
	})
}

func (a AuctionFacadeAdapter) ConfirmSettlement(ctx context.Context, auctionID settlement.AuctionID) error {
	return a.traced(ctx, "facade/auction.ConfirmSettlement", func(ctx context.Context) error {
		return a.facade.ConfirmSettlement(ctx, auctionID.UUID())
	})
}

func (a AuctionFacadeAdapter) traced(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx, span := a.tracer.Start(ctx, name)
	defer span.End()
	err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
