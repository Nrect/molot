package auction

import "context"

// Repository is the minimal aggregate store (rules 15-16, 21, 23).
// Mutations go through update closures: the adapter owns the
// transaction (SELECT ... FOR UPDATE + version), persists the returned
// aggregate, appends bids from recorded BidPlaced events to the
// append-only history and publishes mapped integration events through
// the outbox — all in one transaction.
type Repository interface {
	// Add persists a new aggregate. Idempotent: an id conflict is a
	// silent no-op (ON CONFLICT (id) DO NOTHING — no events re-emitted);
	// a relist_of uniqueness conflict returns ErrAlreadyRelisted.
	Add(ctx context.Context, a *Auction) error

	// Get loads an aggregate. The auction card is public, so no actor
	// is required (documented decision, §2.1).
	Get(ctx context.Context, id AuctionID) (*Auction, error)

	// Update runs a user-driven mutation; the acting user is an
	// explicit typed parameter (rule 21), never context values.
	Update(
		ctx context.Context,
		id AuctionID,
		actor Actor,
		updateFn func(ctx context.Context, a *Auction) (*Auction, error),
	) error

	// UpdateAsSystem is Update for system flows — the closing worker
	// and the settlement saga. The name shouts about the security
	// implication (rule 23).
	UpdateAsSystem(
		ctx context.Context,
		id AuctionID,
		updateFn func(ctx context.Context, a *Auction) (*Auction, error),
	) error
}
