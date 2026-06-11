package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// MarkSaleFailed flips a sold-but-unsettled auction to NotSold.
// Saga-only (facade); "sale already failed" maps to nil (§2.1).
type MarkSaleFailed struct {
	AuctionID auction.AuctionID
	Reason    auction.FailureReason
}

type MarkSaleFailedHandler struct {
	repo  auction.Repository
	clock clock
}

func NewMarkSaleFailedHandler(repo auction.Repository, clock clock) MarkSaleFailedHandler {
	if repo == nil {
		panic("NewMarkSaleFailedHandler: nil repo")
	}
	if clock == nil {
		panic("NewMarkSaleFailedHandler: nil clock")
	}
	return MarkSaleFailedHandler{repo: repo, clock: clock}
}

func (h MarkSaleFailedHandler) Handle(ctx context.Context, cmd MarkSaleFailed) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.AuctionID,
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if err := a.FailSale(cmd.Reason, h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auction.ErrSaleAlreadyFailed):
		return nil // already in the target state — idempotent no-op
	case errors.Is(err, auction.ErrAlreadySettled):
		return errs.NewConflictError("already-settled").WithCause(err)
	case errors.Is(err, auction.ErrInvalidFailureReason):
		return errs.NewIncorrectInputError("invalid-failure-reason").WithCause(err)
	case errors.Is(err, auction.ErrBiddingStillOpen), errors.Is(err, auction.ErrAuctionCancelled):
		return errs.NewConflictError("auction-not-closed").WithCause(err)
	default:
		err, _ = mapNotFound(err)
		return err
	}
}
