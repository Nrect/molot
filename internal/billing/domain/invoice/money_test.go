package invoice_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/domain/invoice"
)

func TestNewCurrency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code    string
		wantErr error
	}{
		{"USD", nil},
		{"EUR", nil},
		{"usd", invoice.ErrInvalidCurrency},
		{"US", invoice.ErrInvalidCurrency},
		{"USDT", invoice.ErrInvalidCurrency},
		{"U1D", invoice.ErrInvalidCurrency},
		{"", invoice.ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run("code "+tc.code, func(t *testing.T) {
			t.Parallel()
			cur, err := invoice.NewCurrency(tc.code)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.code, cur.String())
			assert.False(t, cur.IsZero())
		})
	}
}

func TestNewMoney(t *testing.T) {
	t.Parallel()

	usd, err := invoice.NewCurrency("USD")
	require.NoError(t, err)

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		m, err := invoice.NewMoney(100, usd)
		require.NoError(t, err)
		assert.Equal(t, int64(100), m.Amount())
		assert.Equal(t, usd, m.Currency())
		assert.False(t, m.IsZero())
	})
	t.Run("zero amount is a valid money", func(t *testing.T) {
		t.Parallel()
		m, err := invoice.NewMoney(0, usd)
		require.NoError(t, err)
		assert.False(t, m.IsZero(), "0 USD is money; only the zero VALUE means unset")
	})
	t.Run("negative amount", func(t *testing.T) {
		t.Parallel()
		_, err := invoice.NewMoney(-1, usd)
		assert.ErrorIs(t, err, invoice.ErrNegativeAmount)
	})
	t.Run("zero currency", func(t *testing.T) {
		t.Parallel()
		_, err := invoice.NewMoney(100, invoice.Currency{})
		assert.ErrorIs(t, err, invoice.ErrInvalidCurrency)
	})
}

func TestMoneyAdd(t *testing.T) {
	t.Parallel()

	usd, err := invoice.NewCurrency("USD")
	require.NoError(t, err)
	eur, err := invoice.NewCurrency("EUR")
	require.NoError(t, err)

	a, err := invoice.NewMoney(100, usd)
	require.NoError(t, err)
	b, err := invoice.NewMoney(23, usd)
	require.NoError(t, err)
	c, err := invoice.NewMoney(1, eur)
	require.NoError(t, err)

	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.Equal(t, int64(123), sum.Amount())

	_, err = a.Add(c)
	assert.ErrorIs(t, err, invoice.ErrCurrencyMismatch)
}

func TestMulBasisPointsTruncatesTowardZero(t *testing.T) {
	t.Parallel()

	usd, err := invoice.NewCurrency("USD")
	require.NoError(t, err)

	cases := []struct {
		amount int64
		bp     int
		want   int64
	}{
		{100_000, 1_000, 10_000}, // 10%
		{999, 1_000, 99},         // 99.9 → 99: never rounds up in the platform's favor
		{1, 1, 0},                // 0.0001 → 0
		{100, 10_000, 100},       // 100%
		{100, 0, 0},              // 0%
	}
	for _, tc := range cases {
		m, err := invoice.NewMoney(tc.amount, usd)
		require.NoError(t, err)
		assert.Equal(t, tc.want, m.MulBasisPoints(tc.bp).Amount(),
			"%d minor at %d bp", tc.amount, tc.bp)
	}
}

func TestNewCommissionPolicyAndCommissionFor(t *testing.T) {
	t.Parallel()

	_, err := invoice.NewCommissionPolicy(-1)
	assert.ErrorIs(t, err, invoice.ErrInvalidCommissionRate)
	_, err = invoice.NewCommissionPolicy(10_001)
	assert.ErrorIs(t, err, invoice.ErrInvalidCommissionRate)

	p, err := invoice.NewCommissionPolicy(750) // 7.5%
	require.NoError(t, err)
	assert.Equal(t, 750, p.BasisPoints())

	usd, err := invoice.NewCurrency("USD")
	require.NoError(t, err)
	hammer, err := invoice.NewMoney(200_000, usd)
	require.NoError(t, err)
	commission := invoice.CommissionFor(hammer, p)
	assert.Equal(t, int64(15_000), commission.Amount())
	assert.Equal(t, usd, commission.Currency())
}

func TestSmallValueObjects(t *testing.T) {
	t.Parallel()

	t.Run("attempt", func(t *testing.T) {
		t.Parallel()
		first, err := invoice.NewAttempt(1)
		require.NoError(t, err)
		assert.Equal(t, invoice.AttemptFirst, first)
		second, err := invoice.NewAttempt(2)
		require.NoError(t, err)
		assert.Equal(t, invoice.AttemptSecondChance, second)
		for _, n := range []int{0, 3, -1} {
			_, err := invoice.NewAttempt(n)
			assert.ErrorIs(t, err, invoice.ErrInvalidAttempt, "attempt %d", n)
		}
		assert.True(t, invoice.Attempt{}.IsZero())
	})

	t.Run("payment term", func(t *testing.T) {
		t.Parallel()
		pt, err := invoice.NewPaymentTerm(48 * time.Hour)
		require.NoError(t, err)
		assert.Equal(t, 48*time.Hour, pt.Duration())
		for _, d := range []time.Duration{0, -time.Hour} {
			_, err := invoice.NewPaymentTerm(d)
			assert.ErrorIs(t, err, invoice.ErrInvalidPaymentTerm, "term %s", d)
		}
	})

	t.Run("payment reference", func(t *testing.T) {
		t.Parallel()
		ref, err := invoice.NewPaymentReference("psp-1")
		require.NoError(t, err)
		assert.Equal(t, "psp-1", ref.String())
		assert.False(t, ref.IsZero())
		_, err = invoice.NewPaymentReference("")
		assert.ErrorIs(t, err, invoice.ErrEmptyPaymentReference)
	})

	t.Run("status from string", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{"pending", "paid", "expired", "voided"} {
			status, err := invoice.NewInvoiceStatusFromString(s)
			require.NoError(t, err)
			assert.Equal(t, s, status.String())
			assert.False(t, status.IsZero())
		}
		_, err := invoice.NewInvoiceStatusFromString("cancelled")
		assert.Error(t, err)
		assert.True(t, invoice.InvoiceStatus{}.IsZero())
	})
}
