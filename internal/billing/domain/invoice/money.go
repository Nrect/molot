package invoice

import "strings"

// Currency is an ISO-4217-style alphabetic code. Billing deliberately
// owns its copy of the money value objects (BOOK_AUDIT rule 4): common
// holds zero business types and contexts never share domain code.
type Currency struct {
	code string
}

// NewCurrency validates a 3-letter uppercase ASCII currency code.
func NewCurrency(code string) (Currency, error) {
	if len(code) != 3 {
		return Currency{}, ErrInvalidCurrency
	}
	if strings.ToUpper(code) != code {
		return Currency{}, ErrInvalidCurrency
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return Currency{}, ErrInvalidCurrency
		}
	}
	return Currency{code: code}, nil
}

func (c Currency) String() string { return c.code }

func (c Currency) IsZero() bool { return c == Currency{} }

// Money is an amount in minor units of one currency. The zero value
// means "no money set" — never a valid amount of an unknown currency.
type Money struct {
	amount   int64
	currency Currency
}

// NewMoney builds a non-negative amount of minor units in currency.
func NewMoney(amountMinor int64, currency Currency) (Money, error) {
	if amountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	if currency.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	return Money{amount: amountMinor, currency: currency}, nil
}

func (m Money) Amount() int64 { return m.amount }

func (m Money) Currency() Currency { return m.currency }

func (m Money) IsZero() bool { return m == Money{} }

// Add returns m+other, guarding against mixing currencies.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{amount: m.amount + other.amount, currency: m.currency}, nil
}

// MulBasisPoints returns bp/10000 of m, truncated toward zero —
// the platform never rounds a commission up in its own favor.
func (m Money) MulBasisPoints(bp int) Money {
	return Money{amount: m.amount * int64(bp) / 10_000, currency: m.currency}
}
