package invoice

import (
	"context"
	"time"
)

// Repository is the persistence contract of the Invoice aggregate
// (interface lives in the domain, BOOK_AUDIT rules 15-16, 21, 23).
//
// Security: Get/Update take the acting bidder as an explicit typed
// parameter — identity is never extracted from context inside a repo.
// Anti-enumeration (§2.2): a foreign invoice is indistinguishable from
// a missing one — reads keep ownership in the WHERE predicate, the
// update path runs CanDebtorAccessInvoice inside the transaction and
// maps its violation to NotFoundError (the real forbidden error is
// logged at WARN for the audit trail).
type Repository interface {
	// Add persists a new invoice and publishes its recorded events in
	// the same transaction. Idempotent by UNIQUE(auction_id, attempt)
	// and the deterministic id: a duplicate returns
	// ErrInvoiceAlreadyIssued and publishes nothing.
	Add(ctx context.Context, inv *Invoice) error

	// Get loads the invoice for its debtor, without locking.
	// Missing or foreign → NotFoundError.
	Get(ctx context.Context, id InvoiceID, actor BidderID) (*Invoice, error)

	// Update runs updateFn on the invoice under SELECT ... FOR UPDATE,
	// persists the returned aggregate and publishes its recorded events
	// in the same transaction. An updateFn error rolls everything back
	// and is returned as-is (domain sentinels stay recognizable).
	Update(
		ctx context.Context,
		id InvoiceID,
		actor BidderID,
		updateFn func(ctx context.Context, inv *Invoice) (*Invoice, error),
	) error

	// UpdateAsSystem is Update without an acting user — the name shouts
	// about the security implication (BOOK_AUDIT rule 23). Callers: the
	// expiry worker and the settlement saga facade.
	UpdateAsSystem(
		ctx context.Context,
		id InvoiceID,
		updateFn func(ctx context.Context, inv *Invoice) (*Invoice, error),
	) error

	// PendingDueBefore lists ids of pending invoices with due_at <= t
	// (generic due-scan for the expiry worker, backed by the partial
	// index invoices_due_idx; not a per-use-case query method).
	PendingDueBefore(ctx context.Context, t time.Time, limit int) ([]InvoiceID, error)
}
