package auction

// ListingRules is the platform policy snapshotted onto the aggregate
// once, by the factory, at listing time: the platform currency, the
// verified-bid threshold and the anti-snipe policy. PlaceBid never
// reads platform config (§2.1).
type ListingRules struct {
	currency    Currency
	verifyAbove Money
	antiSnipe   AntiSnipePolicy
}

func NewListingRules(currency Currency, verifyAbove Money, antiSnipe AntiSnipePolicy) (ListingRules, error) {
	if currency.IsZero() {
		return ListingRules{}, ErrInvalidCurrency
	}
	if verifyAbove.IsZero() || verifyAbove.Currency() != currency {
		return ListingRules{}, ErrInvalidListingRules
	}
	if antiSnipe.IsZero() {
		return ListingRules{}, ErrInvalidListingRules
	}
	return ListingRules{currency: currency, verifyAbove: verifyAbove, antiSnipe: antiSnipe}, nil
}

func (r ListingRules) IsZero() bool               { return r == ListingRules{} }
func (r ListingRules) Currency() Currency         { return r.currency }
func (r ListingRules) VerifyAbove() Money         { return r.verifyAbove }
func (r ListingRules) AntiSnipe() AntiSnipePolicy { return r.antiSnipe }
