// Package invoice is the billing context domain: the Invoice aggregate
// (winner's bill = hammer price + platform commission), its value
// objects and the repository contract (ARCHITECTURE.md §2.2).
//
// The package depends on nothing above it; github.com/google/uuid is
// the single allowed identity primitive (typed IDs are validated uuid
// wrappers, §2.1).
package invoice

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// --- typed identities ------------------------------------------------------

// InvoiceID identifies an invoice. The settlement saga derives it
// deterministically (uuidv5 of auctionID+":"+attempt), which together
// with UNIQUE(auction_id, attempt) makes IssueInvoice idempotent.
type InvoiceID struct{ id uuid.UUID }

func NewInvoiceID(id uuid.UUID) (InvoiceID, error) {
	if id == uuid.Nil {
		return InvoiceID{}, errors.New("invoice id must not be nil")
	}
	return InvoiceID{id: id}, nil
}

func (i InvoiceID) UUID() uuid.UUID { return i.id }
func (i InvoiceID) String() string  { return i.id.String() }
func (i InvoiceID) IsZero() bool    { return i == InvoiceID{} }

// AuctionID references the auction the invoice settles. It is billing's
// own type: contexts never import each other's domain.
type AuctionID struct{ id uuid.UUID }

func NewAuctionID(id uuid.UUID) (AuctionID, error) {
	if id == uuid.Nil {
		return AuctionID{}, errors.New("auction id must not be nil")
	}
	return AuctionID{id: id}, nil
}

func (a AuctionID) UUID() uuid.UUID { return a.id }
func (a AuctionID) String() string  { return a.id.String() }
func (a AuctionID) IsZero() bool    { return a == AuctionID{} }

// BidderID identifies the debtor (auction winner or runner-up).
type BidderID struct{ id uuid.UUID }

func NewBidderID(id uuid.UUID) (BidderID, error) {
	if id == uuid.Nil {
		return BidderID{}, errors.New("bidder id must not be nil")
	}
	return BidderID{id: id}, nil
}

func (b BidderID) UUID() uuid.UUID { return b.id }
func (b BidderID) String() string  { return b.id.String() }
func (b BidderID) IsZero() bool    { return b == BidderID{} }

// --- value objects ---------------------------------------------------------

// Attempt is the issue attempt: 1 = original winner, 2 = second chance
// (runner-up). Closed enum (BOOK_AUDIT rule 12).
type Attempt struct{ n int }

var (
	AttemptFirst        = Attempt{n: 1}
	AttemptSecondChance = Attempt{n: 2}
)

func NewAttempt(n int) (Attempt, error) {
	switch n {
	case 1, 2:
		return Attempt{n: n}, nil
	default:
		return Attempt{}, ErrInvalidAttempt
	}
}

func (a Attempt) Int() int     { return a.n }
func (a Attempt) IsZero() bool { return a == Attempt{} }

// PaymentTerm is how long the debtor has to pay: due_at = issued_at + term.
// The deadline belongs to the invoice, not the saga (§6.5).
type PaymentTerm struct{ d time.Duration }

func NewPaymentTerm(d time.Duration) (PaymentTerm, error) {
	if d <= 0 {
		return PaymentTerm{}, ErrInvalidPaymentTerm
	}
	return PaymentTerm{d: d}, nil
}

func (t PaymentTerm) Duration() time.Duration { return t.d }
func (t PaymentTerm) IsZero() bool            { return t == PaymentTerm{} }

// PaymentReference is the PSP's charge reference; zero until paid.
type PaymentReference struct{ ref string }

func NewPaymentReference(ref string) (PaymentReference, error) {
	if ref == "" {
		return PaymentReference{}, ErrEmptyPaymentReference
	}
	return PaymentReference{ref: ref}, nil
}

func (r PaymentReference) String() string { return r.ref }
func (r PaymentReference) IsZero() bool   { return r == PaymentReference{} }

// InvoiceStatus is the closed lifecycle enum of the aggregate.
type InvoiceStatus struct{ s string }

var (
	StatusPending = InvoiceStatus{s: "pending"}
	StatusPaid    = InvoiceStatus{s: "paid"}
	StatusExpired = InvoiceStatus{s: "expired"}
	StatusVoided  = InvoiceStatus{s: "voided"}
)

func NewInvoiceStatusFromString(s string) (InvoiceStatus, error) {
	switch s {
	case StatusPending.s, StatusPaid.s, StatusExpired.s, StatusVoided.s:
		return InvoiceStatus{s: s}, nil
	default:
		return InvoiceStatus{}, errors.New("unknown invoice status: " + s)
	}
}

func (s InvoiceStatus) String() string { return s.s }
func (s InvoiceStatus) IsZero() bool   { return s == InvoiceStatus{} }

// --- aggregate -------------------------------------------------------------

// Invoice is the bill issued to the auction winner (or runner-up on
// second chance): total = hammer + commission, payable until dueAt.
// All fields are unexported; every state change is a behavior method
// that guards its invariant and performs the transition atomically.
type Invoice struct {
	id         InvoiceID
	auctionID  AuctionID
	debtor     BidderID
	hammer     Money
	commission Money
	total      Money
	status     InvoiceStatus
	dueAt      time.Time
	attempt    Attempt
	pspRef     PaymentReference // zero until paid
	version    int64            // optimistic-lock mapping; the adapter increments
	events     []DomainEvent    // recorded domain events, drained by PullDomainEvents
}

// NewInvoice issues a pending invoice: commission and total are
// snapshotted from the policy at issue time, dueAt = now + term.
// Records InvoiceIssued.
func NewInvoice(
	id InvoiceID,
	auctionID AuctionID,
	debtor BidderID,
	hammer Money,
	cp CommissionPolicy,
	pt PaymentTerm,
	attempt Attempt,
	now time.Time,
) (*Invoice, error) {
	switch {
	case id.IsZero():
		return nil, errors.New("invoice id is required")
	case auctionID.IsZero():
		return nil, errors.New("auction id is required")
	case debtor.IsZero():
		return nil, errors.New("debtor is required")
	case hammer.IsZero():
		return nil, errors.New("hammer price is required")
	case pt.IsZero():
		return nil, errors.New("payment term is required")
	case attempt.IsZero():
		return nil, errors.New("attempt is required")
	case now.IsZero():
		return nil, errors.New("current time is required")
	}

	commission := CommissionFor(hammer, cp)
	total, err := hammer.Add(commission)
	if err != nil {
		return nil, err
	}

	now = now.UTC()
	inv := &Invoice{
		id:         id,
		auctionID:  auctionID,
		debtor:     debtor,
		hammer:     hammer,
		commission: commission,
		total:      total,
		status:     StatusPending,
		dueAt:      now.Add(pt.Duration()),
		attempt:    attempt,
		version:    1,
	}
	inv.record(InvoiceIssued{
		InvoiceID:  id,
		AuctionID:  auctionID,
		Debtor:     debtor,
		Hammer:     hammer,
		Commission: commission,
		Total:      total,
		DueAt:      inv.dueAt,
		Attempt:    attempt,
		OccurredAt: now,
	})
	return inv, nil
}

// UnmarshalFromDatabase rehydrates an Invoice from storage. It is the
// only way an adapter may construct the aggregate (BOOK_AUDIT rule 7);
// no events are recorded.
func UnmarshalFromDatabase(
	id InvoiceID,
	auctionID AuctionID,
	debtor BidderID,
	hammer Money,
	commission Money,
	total Money,
	status InvoiceStatus,
	dueAt time.Time,
	attempt Attempt,
	pspRef PaymentReference,
	version int64,
) (*Invoice, error) {
	switch {
	case id.IsZero():
		return nil, errors.New("invoice id is required")
	case auctionID.IsZero():
		return nil, errors.New("auction id is required")
	case debtor.IsZero():
		return nil, errors.New("debtor is required")
	case hammer.IsZero():
		return nil, errors.New("hammer price is required")
	case total.IsZero():
		return nil, errors.New("total is required")
	case status.IsZero():
		return nil, errors.New("status is required")
	case dueAt.IsZero():
		return nil, errors.New("due date is required")
	case attempt.IsZero():
		return nil, errors.New("attempt is required")
	case version < 1:
		return nil, errors.New("version must be at least 1")
	}
	return &Invoice{
		id:         id,
		auctionID:  auctionID,
		debtor:     debtor,
		hammer:     hammer,
		commission: commission,
		total:      total,
		status:     status,
		dueAt:      dueAt.UTC(),
		attempt:    attempt,
		pspRef:     pspRef,
		version:    version,
	}, nil
}

// MarkPaid transitions Pending → Paid. The deadline is deliberately NOT
// enforced here: expiry belongs to the worker, so a payment that lands
// before the Expire transaction commits stays valid (grace = poll
// interval, §2.2 guard table).
func (i *Invoice) MarkPaid(ref PaymentReference, now time.Time) error {
	if ref.IsZero() {
		return ErrEmptyPaymentReference
	}
	switch i.status {
	case StatusPending:
		i.status = StatusPaid
		i.pspRef = ref
		i.record(InvoicePaid{
			InvoiceID:  i.id,
			AuctionID:  i.auctionID,
			Debtor:     i.debtor,
			Total:      i.total,
			Reference:  ref,
			OccurredAt: now.UTC(),
		})
		return nil
	case StatusPaid:
		return ErrInvoiceAlreadyPaid
	case StatusExpired:
		return ErrInvoiceExpired
	case StatusVoided:
		return ErrInvoiceVoided
	default:
		panic("invoice: unknown status " + i.status.s)
	}
}

// Expire transitions Pending → Expired once the deadline has passed
// (the expiry worker's mutation). Already-terminal statuses return
// their sentinel — the worker's handler maps those to nil.
func (i *Invoice) Expire(now time.Time) error {
	switch i.status {
	case StatusPending:
		if now.Before(i.dueAt) {
			return ErrInvoiceNotDue
		}
		i.status = StatusExpired
		i.record(InvoiceExpired{
			InvoiceID:  i.id,
			AuctionID:  i.auctionID,
			Debtor:     i.debtor,
			Attempt:    i.attempt,
			OccurredAt: now.UTC(),
		})
		return nil
	case StatusPaid:
		return ErrInvoiceAlreadyPaid
	case StatusExpired:
		return ErrInvoiceAlreadyExpired
	case StatusVoided:
		return ErrInvoiceVoided
	default:
		panic("invoice: unknown status " + i.status.s)
	}
}

// Void transitions Pending → Voided (saga: the runner-up declined the
// second-chance offer). ErrInvoiceAlreadyPaid is NOT a no-op for
// callers: the payment won and the decline must fail (§2.2).
func (i *Invoice) Void(now time.Time) error {
	switch i.status {
	case StatusPending:
		i.status = StatusVoided
		i.record(InvoiceVoided{InvoiceID: i.id, OccurredAt: now.UTC()})
		return nil
	case StatusPaid:
		return ErrInvoiceAlreadyPaid
	case StatusExpired:
		return ErrInvoiceAlreadyExpired
	case StatusVoided:
		return ErrInvoiceAlreadyVoided
	default:
		panic("invoice: unknown status " + i.status.s)
	}
}

// --- predicates and typed getters (mapping; no setters, no GetX) -----------

func (i Invoice) IsPending() bool { return i.status == StatusPending }

func (i Invoice) ID() InvoiceID            { return i.id }
func (i Invoice) AuctionID() AuctionID     { return i.auctionID }
func (i Invoice) Debtor() BidderID         { return i.debtor }
func (i Invoice) Hammer() Money            { return i.hammer }
func (i Invoice) Commission() Money        { return i.commission }
func (i Invoice) Total() Money             { return i.total }
func (i Invoice) Status() InvoiceStatus    { return i.status }
func (i Invoice) DueAt() time.Time         { return i.dueAt }
func (i Invoice) Attempt() Attempt         { return i.attempt }
func (i Invoice) PSPRef() PaymentReference { return i.pspRef }
func (i Invoice) Version() int64           { return i.version }

// PullDomainEvents drains recorded events for the persisting adapter.
func (i *Invoice) PullDomainEvents() []DomainEvent {
	events := i.events
	i.events = nil
	return events
}

func (i *Invoice) record(e DomainEvent) {
	i.events = append(i.events, e)
}

// CanDebtorAccessInvoice is the pure authorization rule (BOOK_AUDIT
// rule 22): only the debtor may see or pay their invoice. The
// repository calls it inside the update transaction and maps the
// violation to NotFoundError outward (anti-enumeration, §2.2).
func CanDebtorAccessInvoice(actor BidderID, i Invoice) error {
	if actor != i.debtor {
		return ForbiddenInvoiceAccessError{Actor: actor, Debtor: i.debtor}
	}
	return nil
}
