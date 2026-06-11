package auction

// ReservePrice is the seller's hidden minimum. The zero value means
// "no reserve" (IsZero). It is never exposed through events or the
// API — subscribers get ready-made verdicts (RunnerUpQualifies).
type ReservePrice struct {
	price Money
}

// NewReservePrice builds a reserve from a positive Money.
func NewReservePrice(m Money) (ReservePrice, error) {
	if m.IsZero() || m.AmountMinor() <= 0 {
		return ReservePrice{}, ErrInvalidReserve
	}
	return ReservePrice{price: m}, nil
}

// NoReserve is the explicit "no reserve" value.
func NoReserve() ReservePrice { return ReservePrice{} }

func (r ReservePrice) IsZero() bool { return r == ReservePrice{} }

// Money exposes the underlying amount for persistence mapping only.
func (r ReservePrice) Money() Money { return r.price }

// MetBy reports whether amount satisfies the reserve. No reserve is
// met by any amount; a currency mismatch never satisfies it.
func (r ReservePrice) MetBy(amount Money) bool {
	if r.IsZero() {
		return true
	}
	ok, err := amount.GTE(r.price)
	if err != nil {
		return false
	}
	return ok
}
