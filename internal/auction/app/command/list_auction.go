package command

import (
	"context"
	"errors"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// ListAuction publishes a lot for bidding. The platform ListingRules
// (currency + verification threshold + anti-snipe policy) are NOT part
// of the command: the handler snapshots them onto the aggregate at
// listing time (§2.1).
type ListAuction struct {
	AuctionID  auction.AuctionID
	Seller     auction.SellerID
	Lot        auction.Lot
	StartPrice auction.Money
	Increment  auction.Money
	Reserve    auction.ReservePrice
	Window     auction.BiddingWindow
}

type ListAuctionHandler struct {
	repo  auction.Repository
	rules auction.ListingRules
	clock clock
}

func NewListAuctionHandler(repo auction.Repository, rules auction.ListingRules, clock clock) ListAuctionHandler {
	if repo == nil {
		panic("NewListAuctionHandler: nil repo")
	}
	if rules.IsZero() {
		panic("NewListAuctionHandler: zero listing rules")
	}
	if clock == nil {
		panic("NewListAuctionHandler: nil clock")
	}
	return ListAuctionHandler{repo: repo, rules: rules, clock: clock}
}

func (h ListAuctionHandler) Handle(ctx context.Context, cmd ListAuction) error {
	a, err := auction.New(cmd.AuctionID, cmd.Seller, cmd.Lot,
		cmd.StartPrice, cmd.Increment, cmd.Reserve, cmd.Window,
		h.rules, h.clock.Now())
	if err != nil {
		return mapListingError(err)
	}
	// Add is idempotent: a retried POST with the same client-generated
	// id is a silent no-op.
	if err := h.repo.Add(ctx, a); err != nil {
		return err
	}
	return nil
}

func mapListingError(err error) error {
	switch {
	case errors.Is(err, auction.ErrUnsupportedCurrency):
		return errs.NewIncorrectInputError("unsupported-currency").WithCause(err)
	case errors.Is(err, auction.ErrInvalidBiddingWindow):
		return errs.NewIncorrectInputError("invalid-window").WithCause(err)
	case errors.Is(err, auction.ErrInvalidIncrement),
		errors.Is(err, auction.ErrCurrencyMismatch):
		return errs.NewIncorrectInputError("invalid-increment").WithCause(err)
	default:
		return errs.NewIncorrectInputError("invalid-listing").WithCause(err)
	}
}
