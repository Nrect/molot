package settlement

// Money is settlement's own minimal money VO — a deliberate duplicate
// of the auction/billing money concepts (BOOK_AUDIT rule 4: common
// business notions are copied per context, never shared). The saga only
// carries amounts between events and facades, so no arithmetic lives
// here.
type Money struct {
	amount   int64
	currency Currency
}

func NewMoney(amount int64, currency Currency) (Money, error) {
	if amount < 0 {
		return Money{}, ErrNegativeAmount
	}
	if currency.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	return Money{amount: amount, currency: currency}, nil
}

func (m Money) Amount() int64      { return m.amount }
func (m Money) Currency() Currency { return m.currency }
func (m Money) IsZero() bool       { return m == Money{} }

// Currency is a 3-letter uppercase ISO-4217 style code.
type Currency struct{ code string }

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

func (c Currency) String() string { return c.code }
func (c Currency) IsZero() bool   { return c == Currency{} }
