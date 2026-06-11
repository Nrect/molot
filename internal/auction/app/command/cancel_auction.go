package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// CancelAuction withdraws a bidless listing; only its seller may do it.
type CancelAuction struct {
	AuctionID auction.AuctionID
	Seller    auction.SellerID
}

type CancelAuctionHandler struct {
	repo  auction.Repository
	clock clock
}

func NewCancelAuctionHandler(repo auction.Repository, clock clock) CancelAuctionHandler {
	if repo == nil {
		panic("NewCancelAuctionHandler: nil repo")
	}
	if clock == nil {
		panic("NewCancelAuctionHandler: nil clock")
	}
	return CancelAuctionHandler{repo: repo, clock: clock}
}

func (h CancelAuctionHandler) Handle(ctx context.Context, cmd CancelAuction) error {
	err := h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromSeller(cmd.Seller),
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if err := a.Cancel(cmd.Seller, h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	if err != nil {
		return mapCancelError(err)
	}
	return nil
}

func mapCancelError(err error) error {
	if mapped, ok := mapNotFound(err); ok {
		return mapped
	}
	var forbidden auction.ForbiddenAuctionManagementError
	switch {
	case errors.As(err, &forbidden):
		return errs.NewForbiddenError("not-seller").WithCause(err)
	case errors.Is(err, auction.ErrAuctionHasBids):
		return errs.NewConflictError("auction-has-bids").WithCause(err)
	case errors.Is(err, auction.ErrAlreadyClosed):
		return errs.NewConflictError("already-closed").WithCause(err)
	case errors.Is(err, auction.ErrAuctionCancelled):
		return errs.NewConflictError("auction-cancelled").WithCause(err)
	default:
		return err
	}
}
