package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
)

// CloseAuction hammers an auction whose window elapsed. Issued only by
// the ClosingWorker (§10), hence UpdateAsSystem. Idempotency lives in
// the aggregate guards, not in the worker: "already closed", "still
// open" (a bid extended the window after the scan) and "cancelled
// meanwhile" are all benign no-ops for the system flow.
type CloseAuction struct {
	AuctionID auction.AuctionID
}

type CloseAuctionHandler struct {
	repo  auction.Repository
	clock clock
}

func NewCloseAuctionHandler(repo auction.Repository, clock clock) CloseAuctionHandler {
	if repo == nil {
		panic("NewCloseAuctionHandler: nil repo")
	}
	if clock == nil {
		panic("NewCloseAuctionHandler: nil clock")
	}
	return CloseAuctionHandler{repo: repo, clock: clock}
}

func (h CloseAuctionHandler) Handle(ctx context.Context, cmd CloseAuction) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.AuctionID,
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if _, err := a.Close(h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auction.ErrAlreadyClosed),
		errors.Is(err, auction.ErrBiddingStillOpen),
		errors.Is(err, auction.ErrAuctionCancelled):
		// The updateFn error rolled the transaction back; nothing was
		// persisted and no event was published. Benign for the worker.
		return nil
	default:
		err, _ = mapNotFound(err)
		return err
	}
}
