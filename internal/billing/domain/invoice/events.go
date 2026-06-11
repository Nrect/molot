package invoice

import "time"

// DomainEvent is a rich internal event recorded by Invoice behavior
// methods and drained by the repository adapter via PullDomainEvents.
// The adapter maps them to flat integration V1 events and publishes
// through the transactional outbox in the same transaction as the
// aggregate persist (§4.1).
type DomainEvent interface {
	isDomainEvent()
}

// InvoiceIssued is recorded exactly once by NewInvoice.
type InvoiceIssued struct {
	InvoiceID  InvoiceID
	AuctionID  AuctionID
	Debtor     BidderID
	Hammer     Money
	Commission Money
	Total      Money
	DueAt      time.Time
	Attempt    Attempt
	OccurredAt time.Time
}

func (InvoiceIssued) isDomainEvent() {}

// InvoicePaid is recorded by the transaction that actually performed
// the Pending → Paid transition — losers of the Paid-vs-Expired race
// get a guard sentinel and publish nothing (§6.5).
type InvoicePaid struct {
	InvoiceID  InvoiceID
	AuctionID  AuctionID
	Debtor     BidderID
	Total      Money
	Reference  PaymentReference
	OccurredAt time.Time
}

func (InvoicePaid) isDomainEvent() {}

// InvoiceExpired is recorded on the Pending → Expired transition.
type InvoiceExpired struct {
	InvoiceID  InvoiceID
	AuctionID  AuctionID
	Debtor     BidderID
	Attempt    Attempt
	OccurredAt time.Time
}

func (InvoiceExpired) isDomainEvent() {}

// InvoiceVoided deliberately maps to no integration event: void is
// invoked synchronously by the settlement saga and has no other
// consumers (§2.2 — documented absence of speculative code).
type InvoiceVoided struct {
	InvoiceID  InvoiceID
	OccurredAt time.Time
}

func (InvoiceVoided) isDomainEvent() {}
