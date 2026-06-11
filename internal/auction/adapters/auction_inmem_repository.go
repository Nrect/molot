// Package adapters holds the infrastructure side of the auction
// context: repositories (postgres + in-memory), read models,
// projections, the domain→integration event mapper and embedded
// migrations. Each adapter maps explicitly between its storage model
// and the domain (rule 7); no business rules live here (rule 17).
package adapters

import (
	"context"
	"errors"
	"sync"
	"time"

	"molot/internal/auction/domain/auction"
)

// AuctionInMemRepository is the in-memory Repository (rule 20): a map
// of values guarded by an RWMutex; reads return the address of a copy.
// Domain-first development and app tests run against it — semantics
// mirror the postgres adapter (idempotent Add, ErrAlreadyRelisted,
// version increment on update).
type AuctionInMemRepository struct {
	mu       sync.RWMutex
	auctions map[auction.AuctionID]auction.Auction
}

func NewAuctionInMemRepository() *AuctionInMemRepository {
	return &AuctionInMemRepository{auctions: map[auction.AuctionID]auction.Auction{}}
}

func (r *AuctionInMemRepository) Add(_ context.Context, a *auction.Auction) error {
	if a == nil {
		return errors.New("auction must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.auctions[a.ID()]; exists {
		// Same client-generated id: silent no-op, no events re-emitted
		// (mirrors ON CONFLICT (id) DO NOTHING).
		return nil
	}
	if !a.RelistOf().IsZero() {
		for _, existing := range r.auctions {
			if existing.RelistOf() == a.RelistOf() {
				return auction.ErrAlreadyRelisted // mirrors UNIQUE(relist_of)
			}
		}
	}

	a.PullDomainEvents() // the in-memory store has no outbox
	r.auctions[a.ID()] = *a
	return nil
}

func (r *AuctionInMemRepository) Get(_ context.Context, id auction.AuctionID) (*auction.Auction, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stored, ok := r.auctions[id]
	if !ok {
		return nil, auction.NotFoundError{AuctionID: id}
	}
	cp := stored
	return &cp, nil
}

func (r *AuctionInMemRepository) Update(
	ctx context.Context,
	id auction.AuctionID,
	actor auction.Actor,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	if actor.IsZero() {
		return errors.New("update requires an acting user; system flows use UpdateAsSystem")
	}
	return r.update(ctx, id, updateFn)
}

func (r *AuctionInMemRepository) UpdateAsSystem(
	ctx context.Context,
	id auction.AuctionID,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	return r.update(ctx, id, updateFn)
}

// update serializes all mutations behind the write lock, mirroring the
// row lock of the postgres adapter.
func (r *AuctionInMemRepository) update(
	ctx context.Context,
	id auction.AuctionID,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.auctions[id]
	if !ok {
		return auction.NotFoundError{AuctionID: id}
	}
	working := stored // the closure mutates a copy; failure leaves the map untouched

	updated, err := updateFn(ctx, &working)
	if err != nil {
		return err
	}
	updated.PullDomainEvents()

	bumped, err := cloneWithVersion(updated, updated.Version()+1)
	if err != nil {
		return err
	}
	r.auctions[id] = *bumped
	return nil
}

// DueForClosing is the closing worker's candidate scan (§10) — listed
// auctions whose deadline has passed.
func (r *AuctionInMemRepository) DueForClosing(_ context.Context, before time.Time, limit int) ([]auction.AuctionID, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]auction.AuctionID, 0)
	for id, a := range r.auctions {
		if len(ids) >= limit {
			break
		}
		if a.Status() == auction.StatusListed && !a.EndsAt().After(before) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// cloneWithVersion rebuilds the aggregate with a new version through
// the domain unmarshal factory — the version is owned by adapters
// (§2.1), the domain has no setter for it.
func cloneWithVersion(a *auction.Auction, version int64) (*auction.Auction, error) {
	return auction.UnmarshalFromDatabase(
		a.ID(), a.Seller(), a.Lot(),
		a.StartPrice(), a.Increment(), a.Reserve(),
		a.Window(), a.AntiSnipe(), a.VerifyAbove(),
		a.ExtensionsUsed(), a.Status(), a.Outcome(),
		a.LeadingBid(), a.RunnerUpBid(),
		a.WinnerReassigned(), a.BidCount(),
		a.RelistOf(), a.RelistGeneration(), a.Settled(),
		version,
	)
}
