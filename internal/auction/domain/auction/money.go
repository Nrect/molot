package auction

// Currency is a 3-letter ISO-4217-style code value object.
type Currency struct {
	code string
}

// NewCurrency validates a 3-uppercase-letter currency code.
func NewCurrency(code string) (Currency, error) {
	if len(code) != 3 {
		return Currency{}, ErrInvalidCurrency
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return Currency{}, ErrInvalidCurrency
		}
	}
	return Currency{code: code}, nil
}

func (c Currency) IsZero() bool   { return c == Currency{} }
func (c Currency) String() string { return c.code }

// Money is an amount in minor units of a currency. The zero value means
// "no money set" (IsZero), which the aggregate uses instead of pointer
// flags. The type is deliberately duplicated per context (BOOK_AUDIT
// rule 4): this is the auction context's copy.
type Money struct {
	amount   int64
	currency Currency
}

// NewMoney builds Money from minor units; negative amounts and a zero
// currency are rejected. A zero amount is a valid Money (e.g. a
// verification threshold of zero means "verify every bid").
func NewMoney(amountMinor int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	if amountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	return Money{amount: amountMinor, currency: currency}, nil
}

func (m Money) IsZero() bool       { return m == Money{} }
func (m Money) AmountMinor() int64 { return m.amount }
func (m Money) Currency() Currency { return m.currency }

// Add returns m+o, failing on a currency mismatch.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{amount: m.amount + o.amount, currency: m.currency}, nil
}

// GTE reports whether m >= o, failing on a currency mismatch.
func (m Money) GTE(o Money) (bool, error) {
	if m.currency != o.currency {
		return false, ErrCurrencyMismatch
	}
	return m.amount >= o.amount, nil
}

// MulBasisPoints returns m scaled by bp basis points (1bp = 0.01%),
// rounding towards zero.
func (m Money) MulBasisPoints(bp int) Money {
	return Money{amount: m.amount * int64(bp) / 10000, currency: m.currency}
}
