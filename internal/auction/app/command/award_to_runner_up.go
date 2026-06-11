package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// AwardToRunnerUp reassigns the win to the runner-up after a payment
// timeout. Saga-only (facade); "winner already reassigned" maps to nil
// so saga retries and redeliveries are no-ops (§2.1).
type AwardToRunnerUp struct {
	AuctionID auction.AuctionID
}

type AwardToRunnerUpHandler struct {
	repo  auction.Repository
	clock clock
}

func NewAwardToRunnerUpHandler(repo auction.Repository, clock clock) AwardToRunnerUpHandler {
	if repo == nil {
		panic("NewAwardToRunnerUpHandler: nil repo")
	}
	if clock == nil {
		panic("NewAwardToRunnerUpHandler: nil clock")
	}
	return AwardToRunnerUpHandler{repo: repo, clock: clock}
}

func (h AwardToRunnerUpHandler) Handle(ctx context.Context, cmd AwardToRunnerUp) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.AuctionID,
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if _, err := a.AwardToRunnerUp(h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auction.ErrWinnerAlreadyReassigned):
		return nil // already in the target state — idempotent no-op
	case errors.Is(err, auction.ErrNoQualifyingRunnerUp):
		return errs.NewConflictError("no-qualifying-runner-up").WithCause(err)
	case errors.Is(err, auction.ErrAlreadySettled):
		return errs.NewConflictError("already-settled").WithCause(err)
	case errors.Is(err, auction.ErrSaleAlreadyFailed):
		return errs.NewConflictError("sale-already-failed").WithCause(err)
	case errors.Is(err, auction.ErrBiddingStillOpen), errors.Is(err, auction.ErrAuctionCancelled):
		return errs.NewConflictError("auction-not-closed").WithCause(err)
	default:
		err, _ = mapNotFound(err)
		return err
	}
}
