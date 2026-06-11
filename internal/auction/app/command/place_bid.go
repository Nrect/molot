package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// PlaceBid places a bid on an open auction. BidID is client-generated
// (§8); the bidder's verification status comes from the local
// bidder_profiles projection.
type PlaceBid struct {
	AuctionID auction.AuctionID
	BidID     auction.BidID
	Bidder    auction.BidderID
	Amount    auction.Money
}

type PlaceBidHandler struct {
	repo     auction.Repository
	profiles bidderProfiles
	clock    clock
}

func NewPlaceBidHandler(repo auction.Repository, profiles bidderProfiles, clock clock) PlaceBidHandler {
	if repo == nil {
		panic("NewPlaceBidHandler: nil repo")
	}
	if profiles == nil {
		panic("NewPlaceBidHandler: nil profiles")
	}
	if clock == nil {
		panic("NewPlaceBidHandler: nil clock")
	}
	return PlaceBidHandler{repo: repo, profiles: profiles, clock: clock}
}

func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) error {
	bidder, err := h.profiles.BidderByID(ctx, cmd.Bidder)
	if err != nil {
		return err
	}

	err = h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromBidder(cmd.Bidder),
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if _, err := a.PlaceBid(cmd.BidID, bidder, cmd.Amount, h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	if err != nil {
		return mapPlaceBidError(err)
	}
	return nil
}

func mapPlaceBidError(err error) error {
	if mapped, ok := mapNotFound(err); ok {
		return mapped
	}
	switch {
	case errors.Is(err, auction.ErrAuctionNotOpen):
		return errs.NewConflictError("auction-not-open").WithCause(err)
	case errors.Is(err, auction.ErrSellerCannotBid):
		return errs.NewForbiddenError("seller-cannot-bid").WithCause(err)
	case errors.Is(err, auction.ErrLeaderCannotOutbidSelf):
		return errs.NewConflictError("leader-cannot-outbid-self").WithCause(err)
	case errors.Is(err, auction.ErrCurrencyMismatch):
		return errs.NewIncorrectInputError("currency-mismatch").WithCause(err)
	case errors.Is(err, auction.ErrBidBelowMinimum):
		return errs.NewConflictError("bid-below-minimum").WithCause(err)
	case errors.Is(err, auction.ErrVerificationRequired):
		return errs.NewForbiddenError("verification-required").WithCause(err)
	case errors.Is(err, auction.ErrInvalidBid), errors.Is(err, auction.ErrInvalidID):
		return errs.NewIncorrectInputError("invalid-bid").WithCause(err)
	default:
		return err
	}
}
