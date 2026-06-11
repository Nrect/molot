package adapters

import (
	"context"
	"fmt"
	"sync"

	"molot/internal/settlement/domain/settlement"
)

// SettlementInMemoryRepository is the in-memory settlement.Repository
// (rule 20): a map of aggregate VALUES guarded by an RWMutex; reads
// hand out the address of a copy, so callers can never mutate the
// store without going through Update. It mirrors every behavioral
// contract of the Postgres adapter — including the silent
// ON CONFLICT DO NOTHING semantics of Add — so domain-first development
// and the shared repository suite run against it without Docker.
type SettlementInMemoryRepository struct {
	mu        sync.RWMutex
	byAuction map[settlement.AuctionID]settlement.Settlement
}

func NewSettlementInMemoryRepository() *SettlementInMemoryRepository {
	return &SettlementInMemoryRepository{byAuction: map[settlement.AuctionID]settlement.Settlement{}}
}

func (r *SettlementInMemoryRepository) Add(_ context.Context, s *settlement.Settlement) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byAuction[s.AuctionID()]; ok {
		return nil // INSERT ... ON CONFLICT (auction_id) DO NOTHING parity (§6.1)
	}
	r.byAuction[s.AuctionID()] = *s
	return nil
}

func (r *SettlementInMemoryRepository) Get(_ context.Context, id settlement.AuctionID) (*settlement.Settlement, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stored, ok := r.byAuction[id]
	if !ok {
		return nil, settlement.NotFoundError{AuctionID: id}
	}
	cp := stored
	return &cp, nil
}

func (r *SettlementInMemoryRepository) Update(
	ctx context.Context,
	id settlement.AuctionID,
	updateFn func(ctx context.Context, s *settlement.Settlement) (*settlement.Settlement, error),
) error {
	// The write lock serializes read-modify-write like FOR UPDATE does.
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.byAuction[id]
	if !ok {
		return settlement.NotFoundError{AuctionID: id}
	}

	// updateFn works on a copy: an error rolls back naturally because
	// the store is only replaced on success.
	work := stored
	updated, err := updateFn(ctx, &work)
	if err != nil {
		return err
	}

	// The adapter owns the version increment (§2.4) — mirror the
	// Postgres `version = version + 1` for suite parity.
	bumped, err := settlement.UnmarshalFromDatabase(
		updated.AuctionID(), updated.State(), updated.Winner(), updated.Hammer(),
		updated.RunnerUp(), updated.RunnerUpAmount(), updated.RunnerUpQualifies(),
		updated.RelistGeneration(), updated.Attempt(), updated.InvoiceID(),
		updated.FailureReason(), updated.Version()+1,
	)
	if err != nil {
		return fmt.Errorf("unable to bump settlement version: %w", err)
	}
	r.byAuction[id] = *bumped
	return nil
}
