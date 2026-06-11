package settlement_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/domain/settlement"
)

func TestClosedEnums(t *testing.T) {
	t.Parallel()

	t.Run("state parses and classifies", func(t *testing.T) {
		t.Parallel()
		s, err := settlement.NewStateFromString("second_chance_payment")
		require.NoError(t, err)
		assert.Equal(t, settlement.StateSecondChancePayment, s)
		assert.False(t, s.IsTerminal())
		assert.True(t, settlement.StateRelisted.IsTerminal())

		_, err = settlement.NewStateFromString("paid")
		assert.Error(t, err)
	})

	t.Run("zero state panics on classification", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() { _ = (settlement.State{}).IsTerminal() })
	})

	t.Run("failure reason parses", func(t *testing.T) {
		t.Parallel()
		r, err := settlement.NewFailureReasonFromString("second_chance_declined")
		require.NoError(t, err)
		assert.Equal(t, settlement.ReasonSecondChanceDeclined, r)

		_, err = settlement.NewFailureReasonFromString("buyer-remorse")
		assert.Error(t, err)
	})

	t.Run("next step names are closed", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "relist", settlement.StepRelist.String())
		assert.Panics(t, func() { _ = settlement.NextStep(0).String() })
	})

	t.Run("outcome decides whether to settle", func(t *testing.T) {
		t.Parallel()
		sold, err := settlement.NewOutcomeFromString("sold")
		require.NoError(t, err)
		assert.True(t, sold.RequiresSettlement())

		notSold, err := settlement.NewOutcomeFromString("not_sold")
		require.NoError(t, err)
		assert.False(t, notSold.RequiresSettlement())

		_, err = settlement.NewOutcomeFromString("withdrawn")
		assert.Error(t, err)
		assert.Panics(t, func() { _ = (settlement.Outcome{}).RequiresSettlement() })
	})
}

func TestMoneyAndCurrency(t *testing.T) {
	t.Parallel()

	cur, err := settlement.NewCurrency("EUR")
	require.NoError(t, err)

	_, err = settlement.NewMoney(-1, cur)
	assert.ErrorIs(t, err, settlement.ErrNegativeAmount)

	_, err = settlement.NewMoney(10, settlement.Currency{})
	assert.ErrorIs(t, err, settlement.ErrInvalidCurrency)

	for _, bad := range []string{"eur", "EU", "EURO", "EU1"} {
		_, err := settlement.NewCurrency(bad)
		assert.ErrorIs(t, err, settlement.ErrInvalidCurrency, "currency %q must be rejected", bad)
	}
}

func TestNewClosingValidation(t *testing.T) {
	t.Parallel()

	auctionID, winner := newAuctionID(t), newBidderID(t)
	hammer := money(t, 100_000)
	firstInvoice := newInvoiceID(t)

	cases := []struct {
		name  string
		build func(t *testing.T) error
	}{
		{
			name: "winner is required",
			build: func(t *testing.T) error {
				_, err := settlement.NewClosing(auctionID, settlement.BidderID{}, hammer,
					settlement.BidderID{}, settlement.Money{}, false, 0, firstInvoice)
				return err
			},
		},
		{
			name: "hammer is required",
			build: func(t *testing.T) error {
				_, err := settlement.NewClosing(auctionID, winner, settlement.Money{},
					settlement.BidderID{}, settlement.Money{}, false, 0, firstInvoice)
				return err
			},
		},
		{
			name: "first invoice id is required",
			build: func(t *testing.T) error {
				_, err := settlement.NewClosing(auctionID, winner, hammer,
					settlement.BidderID{}, settlement.Money{}, false, 0, settlement.InvoiceID{})
				return err
			},
		},
		{
			name: "qualification needs a runner-up",
			build: func(t *testing.T) error {
				_, err := settlement.NewClosing(auctionID, winner, hammer,
					settlement.BidderID{}, settlement.Money{}, true, 0, firstInvoice)
				return err
			},
		},
		{
			name: "runner-up needs an amount",
			build: func(t *testing.T) error {
				_, err := settlement.NewClosing(auctionID, winner, hammer,
					newBidderID(t), settlement.Money{}, false, 0, firstInvoice)
				return err
			},
		},
		{
			name: "relist generation must not be negative",
			build: func(t *testing.T) error {
				_, err := settlement.NewClosing(auctionID, winner, hammer,
					settlement.BidderID{}, settlement.Money{}, false, -1, firstInvoice)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Error(t, tc.build(t))
		})
	}
}

func TestRelistPolicy(t *testing.T) {
	t.Parallel()

	policy, err := settlement.NewRelistPolicy(time.Hour, 24*time.Hour)
	require.NoError(t, err)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	startsAt, endsAt := policy.Window(now)
	assert.Equal(t, now.Add(time.Hour), startsAt)
	assert.Equal(t, now.Add(25*time.Hour), endsAt)

	_, err = settlement.NewRelistPolicy(0, time.Hour)
	assert.Error(t, err)
	_, err = settlement.NewRelistPolicy(time.Hour, 0)
	assert.Error(t, err)
}
