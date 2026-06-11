package settlement_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/domain/settlement"
)

// Fixtures go through the public domain API only (rule 40).

func newAuctionID(t *testing.T) settlement.AuctionID {
	t.Helper()
	id, err := settlement.NewAuctionID(uuid.New())
	require.NoError(t, err)
	return id
}

func newBidderID(t *testing.T) settlement.BidderID {
	t.Helper()
	id, err := settlement.NewBidderID(uuid.New())
	require.NoError(t, err)
	return id
}

func newInvoiceID(t *testing.T) settlement.InvoiceID {
	t.Helper()
	id, err := settlement.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	return id
}

func money(t *testing.T, amount int64) settlement.Money {
	t.Helper()
	cur, err := settlement.NewCurrency("EUR")
	require.NoError(t, err)
	m, err := settlement.NewMoney(amount, cur)
	require.NoError(t, err)
	return m
}

type closingOpts struct {
	noRunnerUp bool
	qualifies  bool
	relistGen  int
}

func newClosing(t *testing.T, opts closingOpts) settlement.Closing {
	t.Helper()
	var runnerUp settlement.BidderID
	var runnerUpAmount settlement.Money
	if !opts.noRunnerUp {
		runnerUp = newBidderID(t)
		runnerUpAmount = money(t, 90_000)
	}
	c, err := settlement.NewClosing(
		newAuctionID(t), newBidderID(t), money(t, 100_000),
		runnerUp, runnerUpAmount, opts.qualifies, opts.relistGen, newInvoiceID(t),
	)
	require.NoError(t, err)
	return c
}

// startedSaga is a saga in StateStarted built from a sold closing.
func startedSaga(t *testing.T, opts closingOpts) *settlement.Settlement {
	t.Helper()
	s, err := settlement.Start(newClosing(t, opts))
	require.NoError(t, err)
	return s
}

// awaitingSaga is a saga advanced to StateAwaitingPayment.
func awaitingSaga(t *testing.T, opts closingOpts) *settlement.Settlement {
	t.Helper()
	s := startedSaga(t, opts)
	require.NoError(t, s.InvoiceIssued(s.InvoiceID()))
	return s
}

// secondChanceSaga is a saga advanced to StateSecondChancePayment; the
// returned invoice is the attempt-2 one the saga now awaits.
func secondChanceSaga(t *testing.T, opts closingOpts) (*settlement.Settlement, settlement.InvoiceID) {
	t.Helper()
	opts.qualifies = true
	s := awaitingSaga(t, opts)
	require.NoError(t, s.ApplyNextStep(settlement.StepAwardRunnerUp, settlement.FailureReason{}))
	require.NoError(t, s.RunnerUpAwarded(s.RunnerUp(), s.RunnerUpAmount()))
	inv2 := newInvoiceID(t)
	require.NoError(t, s.SecondChanceInvoiceIssued(inv2))
	return s, inv2
}

// settledSaga is a saga paid on the first attempt.
func settledSaga(t *testing.T, opts closingOpts) *settlement.Settlement {
	t.Helper()
	s := awaitingSaga(t, opts)
	require.NoError(t, s.PaymentReceived(s.InvoiceID()))
	return s
}
