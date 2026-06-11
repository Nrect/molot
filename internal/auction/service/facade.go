package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"molot/internal/auction/app/command"
	"molot/internal/auction/domain/auction"
	"molot/internal/common/decorator"
	"molot/internal/common/errs"
)

// Facade is the synchronous command surface the settlement saga calls
// through its consumer-side auctionGateway (§6, ADR-0004). Signatures
// are deliberately primitive (uuid.UUID, time.Time, string) so the
// settlement adapter needs no auction domain types. Every command is
// idempotent: "already in the target state" returns nil (§2.1), making
// saga retries and redeliveries safe by construction.
type Facade struct {
	award      decorator.CommandHandler[command.AwardToRunnerUp]
	relist     decorator.CommandHandler[command.RelistAuction]
	markFailed decorator.CommandHandler[command.MarkSaleFailed]
	confirm    decorator.CommandHandler[command.ConfirmSettlement]
}

// AwardToRunnerUp promotes the runner-up to winner after a payment
// timeout. Repeat call → nil (ErrWinnerAlreadyReassigned).
func (f Facade) AwardToRunnerUp(ctx context.Context, auctionID uuid.UUID) error {
	id, err := auction.NewAuctionID(auctionID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	return f.award.Handle(ctx, command.AwardToRunnerUp{AuctionID: id})
}

// Relist creates the replacement listing with the saga's deterministic
// newID and the [startsAt, endsAt) bidding window. Repeat call → nil
// (id conflict or UNIQUE(relist_of) → ErrAlreadyRelisted).
func (f Facade) Relist(ctx context.Context, originalID, newID uuid.UUID, startsAt, endsAt time.Time) error {
	origID, err := auction.NewAuctionID(originalID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	relistID, err := auction.NewAuctionID(newID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	window, err := auction.NewBiddingWindow(startsAt, endsAt)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-window").WithCause(err)
	}
	return f.relist.Handle(ctx, command.RelistAuction{
		OriginalID: origID,
		NewID:      relistID,
		Window:     window,
	})
}

// MarkSaleFailed flips the sale to NotSold with a reason
// ("payment_timeout" | "second_chance_declined"). Repeat call → nil
// (ErrSaleAlreadyFailed).
func (f Facade) MarkSaleFailed(ctx context.Context, auctionID uuid.UUID, reason string) error {
	id, err := auction.NewAuctionID(auctionID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	failureReason, err := auction.NewFailureReasonFromString(reason)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-failure-reason").WithCause(err)
	}
	return f.markFailed.Handle(ctx, command.MarkSaleFailed{AuctionID: id, Reason: failureReason})
}

// ConfirmSettlement marks the sale as paid. Repeat call → nil
// (ErrAlreadySettled).
func (f Facade) ConfirmSettlement(ctx context.Context, auctionID uuid.UUID) error {
	id, err := auction.NewAuctionID(auctionID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	return f.confirm.Handle(ctx, command.ConfirmSettlement{AuctionID: id})
}
