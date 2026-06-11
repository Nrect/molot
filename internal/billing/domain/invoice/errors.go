package invoice

import "errors"

// Sentinel errors of the Invoice aggregate (ARCHITECTURE.md §2.2).
// The guard table of MarkPaid/Expire/Void is exhaustive; "→ nil" rows
// are the caller's (command handler's) interpretation, never the
// domain's — the domain always reports the precise reason.
var (
	// ErrInvoiceAlreadyPaid: the invoice reached Paid before this
	// transition. For MarkPaid it means an idempotent duplicate of the
	// same payment; for Void it means payment won and the caller MUST
	// NOT treat it as a no-op.
	ErrInvoiceAlreadyPaid = errors.New("invoice is already paid")
	// ErrInvoiceExpired: MarkPaid lost the race against expiry.
	ErrInvoiceExpired = errors.New("invoice is expired")
	// ErrInvoiceAlreadyExpired: Expire/Void hit an already expired invoice.
	ErrInvoiceAlreadyExpired = errors.New("invoice is already expired")
	// ErrInvoiceVoided: MarkPaid/Expire hit a voided invoice.
	ErrInvoiceVoided = errors.New("invoice is voided")
	// ErrInvoiceAlreadyVoided: Void hit an already voided invoice.
	ErrInvoiceAlreadyVoided = errors.New("invoice is already voided")
	// ErrInvoiceNotDue: Expire called before due_at — worker scan raced
	// a clock skew; the invoice stays pending.
	ErrInvoiceNotDue = errors.New("invoice is not due yet")
	// ErrInvoiceAlreadyIssued: Add hit UNIQUE(auction_id, attempt) or
	// the deterministic invoice id — IssueInvoice maps it to nil.
	ErrInvoiceAlreadyIssued = errors.New("invoice is already issued")

	// PSP outcomes surfaced by the payment gateway port (§3.1).
	ErrPaymentDeclined = errors.New("payment declined by psp")
	ErrPSPUnavailable  = errors.New("psp is unavailable")

	// Value-object validation sentinels.
	ErrCurrencyMismatch      = errors.New("money currency mismatch")
	ErrNegativeAmount        = errors.New("money amount must not be negative")
	ErrInvalidCurrency       = errors.New("currency must be a 3-letter uppercase code")
	ErrInvalidCommissionRate = errors.New("commission rate must be between 0 and 10000 basis points")
	ErrInvalidAttempt        = errors.New("attempt must be 1 or 2")
	ErrInvalidPaymentTerm    = errors.New("payment term must be positive")
	ErrEmptyPaymentReference = errors.New("payment reference must not be empty")
)

// NotFoundError is what every caller sees for both a missing invoice
// and a foreign one: anti-enumeration makes them indistinguishable
// (§2.2 — ownership in the WHERE predicate on reads, access check
// mapped to NotFoundError on the update path).
type NotFoundError struct {
	InvoiceID InvoiceID
}

func (e NotFoundError) Error() string {
	return "invoice " + e.InvoiceID.String() + " not found"
}

// ForbiddenInvoiceAccessError is the real authorization violation. It
// never leaves the process: the repository logs it at WARN (audit
// trail) and returns NotFoundError outward.
type ForbiddenInvoiceAccessError struct {
	Actor  BidderID
	Debtor BidderID
}

func (e ForbiddenInvoiceAccessError) Error() string {
	return "bidder " + e.Actor.String() + " may not access invoice of debtor " + e.Debtor.String()
}
