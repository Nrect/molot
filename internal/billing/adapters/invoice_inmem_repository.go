package adapters

import (
	"context"
	"sort"
	"sync"
	"time"

	"molot/internal/billing/domain/invoice"
)

// InvoiceInMemoryRepository is the in-memory invoice.Repository
// (BOOK_AUDIT rule 20): a map of aggregate VALUES guarded by an
// RWMutex; reads hand out the address of a copy, so callers can never
// mutate the store without going through Update. It mirrors every
// behavioral contract of the Postgres adapter, including
// anti-enumeration on the update path; domain-first development and
// the shared repository suite run against it without Docker.
type InvoiceInMemoryRepository struct {
	mu   sync.RWMutex
	byID map[invoice.InvoiceID]invoice.Invoice
}

func NewInvoiceInMemoryRepository() *InvoiceInMemoryRepository {
	return &InvoiceInMemoryRepository{byID: map[invoice.InvoiceID]invoice.Invoice{}}
}

func (r *InvoiceInMemoryRepository) Add(_ context.Context, inv *invoice.Invoice) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byID[inv.ID()]; ok {
		return invoice.ErrInvoiceAlreadyIssued
	}
	for _, existing := range r.byID {
		if existing.AuctionID() == inv.AuctionID() && existing.Attempt() == inv.Attempt() {
			return invoice.ErrInvoiceAlreadyIssued // UNIQUE(auction_id, attempt) parity
		}
	}
	// Mirror the outbox drain: events are pulled by the persisting
	// adapter, so the stored value carries none.
	inv.PullDomainEvents()
	r.byID[inv.ID()] = *inv
	return nil
}

func (r *InvoiceInMemoryRepository) Get(_ context.Context, id invoice.InvoiceID, actor invoice.BidderID) (*invoice.Invoice, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stored, ok := r.byID[id]
	// Ownership-in-WHERE parity: foreign == missing (anti-enumeration).
	if !ok || invoice.CanDebtorAccessInvoice(actor, stored) != nil {
		return nil, invoice.NotFoundError{InvoiceID: id}
	}
	cp := stored
	return &cp, nil
}

func (r *InvoiceInMemoryRepository) Update(
	ctx context.Context,
	id invoice.InvoiceID,
	actor invoice.BidderID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	return r.update(ctx, id, &actor, updateFn)
}

func (r *InvoiceInMemoryRepository) UpdateAsSystem(
	ctx context.Context,
	id invoice.InvoiceID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	return r.update(ctx, id, nil, updateFn)
}

func (r *InvoiceInMemoryRepository) update(
	ctx context.Context,
	id invoice.InvoiceID,
	actor *invoice.BidderID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	// The write lock serializes read-modify-write like FOR UPDATE does.
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.byID[id]
	if !ok {
		return invoice.NotFoundError{InvoiceID: id}
	}
	if actor != nil {
		if err := invoice.CanDebtorAccessInvoice(*actor, stored); err != nil {
			return invoice.NotFoundError{InvoiceID: id} // anti-enumeration parity
		}
	}

	// updateFn works on a copy: an error rolls back naturally because
	// the store is only replaced on success.
	work := stored
	updated, err := updateFn(ctx, &work)
	if err != nil {
		return err
	}
	updated.PullDomainEvents() // outbox drain parity
	r.byID[id] = *updated
	return nil
}

func (r *InvoiceInMemoryRepository) PendingDueBefore(_ context.Context, t time.Time, limit int) ([]invoice.InvoiceID, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type due struct {
		id    invoice.InvoiceID
		dueAt time.Time
	}
	var candidates []due
	for id, inv := range r.byID {
		if inv.IsPending() && !inv.DueAt().After(t) {
			candidates = append(candidates, due{id: id, dueAt: inv.DueAt()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].dueAt.Before(candidates[j].dueAt) })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	ids := make([]invoice.InvoiceID, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.id)
	}
	return ids, nil
}
