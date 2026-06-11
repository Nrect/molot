package invoice

// CommissionPolicy is the platform's cut expressed in basis points
// (1 bp = 0.01%). It comes from service configuration — the invoice
// snapshots the resulting amounts at issue time, so a later policy
// change never alters already issued invoices.
type CommissionPolicy struct {
	basisPoints int
}

// NewCommissionPolicy validates the rate: 0..10000 bp (0%..100%).
func NewCommissionPolicy(basisPoints int) (CommissionPolicy, error) {
	if basisPoints < 0 || basisPoints > 10_000 {
		return CommissionPolicy{}, ErrInvalidCommissionRate
	}
	return CommissionPolicy{basisPoints: basisPoints}, nil
}

func (p CommissionPolicy) BasisPoints() int { return p.basisPoints }

// CommissionFor is a stateless calculation, deliberately separate from
// aggregate mutation (BOOK_AUDIT rule 13): the platform commission for
// a given hammer price under policy p.
func CommissionFor(hammer Money, p CommissionPolicy) Money {
	return hammer.MulBasisPoints(p.basisPoints)
}
