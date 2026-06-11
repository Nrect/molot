package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// ConfirmSettlement marks a sold auction as paid. Saga-only (facade);
// "already settled" maps to nil (§2.1).
type ConfirmSettlement struct {
	AuctionID auction.AuctionID
}

type ConfirmSettlementHandler struct {
	repo  auction.Repository
	clock clock
}

func NewConfirmSettlementHandler(repo auction.Repository, clock clock) ConfirmSettlementHandler {
	if repo == nil {
		panic("NewConfirmSettlementHandler: nil repo")
	}
	if clock == nil {
		panic("NewConfirmSettlementHandler: nil clock")
	}
	return ConfirmSettlementHandler{repo: repo, clock: clock}
}

func (h ConfirmSettlementHandler) Handle(ctx context.Context, cmd ConfirmSettlement) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.AuctionID,
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if err := a.ConfirmSettlement(h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auction.ErrAlreadySettled):
		return nil // already in the target state — idempotent no-op
	case errors.Is(err, auction.ErrSaleAlreadyFailed):
		return errs.NewConflictError("sale-already-failed").WithCause(err)
	case errors.Is(err, auction.ErrBiddingStillOpen), errors.Is(err, auction.ErrAuctionCancelled):
		return errs.NewConflictError("auction-not-closed").WithCause(err)
	default:
		err, _ = mapNotFound(err)
		return err
	}
}
