package settlement

import "errors"

// Sentinel errors of the settlement saga (§2.4, §6).
var (
	// ErrUnexpectedTransition: the saga is already past (or not yet at)
	// the step this input drives. Every at-least-once duplicate and
	// every lost commit race lands here; handlers treat it as an ack
	// (§6.1) — the effects were idempotent.
	ErrUnexpectedTransition = errors.New("unexpected settlement transition")

	// ErrNotOfferRecipient: DeclineSecondChanceOffer by an actor who
	// does not hold the live second-chance offer (→ 403
	// not-offer-recipient, §6.4).
	ErrNotOfferRecipient = errors.New("actor is not the second-chance offer recipient")

	// ErrOfferAlreadyPaid: the decline lost to the payment — the
	// second-chance invoice is already paid and the sale settles (→ 409
	// offer-already-paid, §6.4).
	ErrOfferAlreadyPaid = errors.New("second-chance offer is already paid")

	// ErrWinnerMismatch: WinnerReassignedV1 carries a bidder or price
	// that does not match the recorded runner-up. This is a
	// cross-context anomaly, never a benign duplicate — it is NOT
	// acked, so retries take it to the dead letter for an operator
	// (§6.8).
	ErrWinnerMismatch = errors.New("reassigned winner does not match the recorded runner-up")

	// Value-object validation sentinels.
	ErrNegativeAmount  = errors.New("money amount must not be negative")
	ErrInvalidCurrency = errors.New("currency must be a 3-letter uppercase code")
)

// NotFoundError: no settlement saga exists for the auction.
type NotFoundError struct {
	AuctionID AuctionID
}

func (e NotFoundError) Error() string {
	return "settlement for auction " + e.AuctionID.String() + " not found"
}
