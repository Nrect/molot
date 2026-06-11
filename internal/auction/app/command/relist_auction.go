package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// RelistAuction creates the replacement listing for a failed sale.
// Saga-only (facade). NewID is deterministic on the saga side
// (uuidv5 of the original id), so a retry collides on the id or on
// UNIQUE(relist_of) — both map to nil here (§2.1).
type RelistAuction struct {
	OriginalID auction.AuctionID
	NewID      auction.AuctionID
	Window     auction.BiddingWindow
}

type RelistAuctionHandler struct {
	repo  auction.Repository
	clock clock
}

func NewRelistAuctionHandler(repo auction.Repository, clock clock) RelistAuctionHandler {
	if repo == nil {
		panic("NewRelistAuctionHandler: nil repo")
	}
	if clock == nil {
		panic("NewRelistAuctionHandler: nil clock")
	}
	return RelistAuctionHandler{repo: repo, clock: clock}
}

func (h RelistAuctionHandler) Handle(ctx context.Context, cmd RelistAuction) error {
	orig, err := h.repo.Get(ctx, cmd.OriginalID)
	if err != nil {
		err, _ = mapNotFound(err)
		return err
	}

	relisted, err := auction.RelistedFrom(orig, cmd.NewID, cmd.Window, h.clock.Now())
	if err != nil {
		return mapRelistError(err)
	}

	// Add: id conflict is a silent no-op; a relist_of uniqueness
	// conflict surfaces as ErrAlreadyRelisted — both mean "already
	// done" for the saga.
	if err := h.repo.Add(ctx, relisted); err != nil {
		return mapRelistError(err)
	}
	return nil
}

func mapRelistError(err error) error {
	switch {
	case errors.Is(err, auction.ErrAlreadyRelisted):
		return nil // already in the target state — idempotent no-op
	case errors.Is(err, auction.ErrRelistLimitReached):
		return errs.NewConflictError("relist-limit-reached").WithCause(err)
	case errors.Is(err, auction.ErrInvalidBiddingWindow):
		return errs.NewIncorrectInputError("invalid-window").WithCause(err)
	default:
		return err
	}
}
