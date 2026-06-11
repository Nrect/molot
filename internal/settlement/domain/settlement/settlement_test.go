package settlement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/domain/settlement"
)

func TestStart(t *testing.T) {
	t.Parallel()

	t.Run("starts at started with attempt 1 and the first invoice recorded", func(t *testing.T) {
		t.Parallel()
		s := startedSaga(t, closingOpts{qualifies: true})

		assert.Equal(t, settlement.StateStarted, s.State())
		assert.Equal(t, 1, s.Attempt())
		assert.False(t, s.InvoiceID().IsZero(), "the deterministic attempt-1 invoice is recorded at birth")
		assert.True(t, s.RunnerUpQualifies())
		assert.True(t, s.FailureReason().IsZero())
		assert.Equal(t, int64(1), s.Version())
	})

	t.Run("rejects a zero closing", func(t *testing.T) {
		t.Parallel()
		_, err := settlement.Start(settlement.Closing{})
		assert.Error(t, err)
	})
}

// TestStateMachine is the saga transition table (§6.3) — commit-phase
// methods with their guards, including both fast-forward seams.
func TestStateMachine(t *testing.T) {
	t.Parallel()

	noReason := settlement.FailureReason{}

	cases := []struct {
		name      string
		arrange   func(t *testing.T) *settlement.Settlement
		act       func(t *testing.T, s *settlement.Settlement) error
		wantErr   error            // nil = transition must succeed
		wantState settlement.State // asserted when wantErr is nil
		after     func(t *testing.T, s *settlement.Settlement)
	}{
		{
			name:    "invoice issued moves started to awaiting payment",
			arrange: func(t *testing.T) *settlement.Settlement { return startedSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.InvoiceIssued(s.InvoiceID())
			},
			wantState: settlement.StateAwaitingPayment,
		},
		{
			name:    "invoice issued again is an acked duplicate",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.InvoiceIssued(s.InvoiceID())
			},
			wantErr: settlement.ErrUnexpectedTransition,
		},
		{
			name:    "payment received settles awaiting payment",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.PaymentReceived(s.InvoiceID())
			},
			wantState: settlement.StateSettled,
		},
		{
			name:    "payment received at started fast-forwards to settled",
			arrange: func(t *testing.T) *settlement.Settlement { return startedSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				// The matching deterministic attempt-1 invoice proves
				// IssueInvoice happened — the lost AwaitingPayment
				// commit is skipped (§6.3).
				return s.PaymentReceived(s.InvoiceID())
			},
			wantState: settlement.StateSettled,
		},
		{
			name:    "payment received for a foreign invoice is an acked duplicate",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.PaymentReceived(newInvoiceID(t))
			},
			wantErr: settlement.ErrUnexpectedTransition,
		},
		{
			name:    "payment received at a terminal state is an acked duplicate",
			arrange: func(t *testing.T) *settlement.Settlement { return settledSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.PaymentReceived(s.InvoiceID())
			},
			wantErr: settlement.ErrUnexpectedTransition,
		},
		{
			name:    "award step moves awaiting payment to awarding runner-up",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{qualifies: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.ApplyNextStep(settlement.StepAwardRunnerUp, noReason)
			},
			wantState: settlement.StateAwardingRunnerUp,
		},
		{
			name:    "award step at started fast-forwards",
			arrange: func(t *testing.T) *settlement.Settlement { return startedSaga(t, closingOpts{qualifies: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.ApplyNextStep(settlement.StepAwardRunnerUp, noReason)
			},
			wantState: settlement.StateAwardingRunnerUp,
		},
		{
			name:    "award step refuses a failure reason",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{qualifies: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				err := s.ApplyNextStep(settlement.StepAwardRunnerUp, settlement.ReasonPaymentTimeout)
				assert.Error(t, err)
				assert.NotErrorIs(t, err, settlement.ErrUnexpectedTransition, "a programming error must not be acked")
				return nil
			},
			wantState: settlement.StateAwaitingPayment,
		},
		{
			name: "runner-up awarded switches the debtor to the runner-up",
			arrange: func(t *testing.T) *settlement.Settlement {
				s := awaitingSaga(t, closingOpts{qualifies: true})
				require.NoError(t, s.ApplyNextStep(settlement.StepAwardRunnerUp, noReason))
				return s
			},
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.RunnerUpAwarded(s.RunnerUp(), s.RunnerUpAmount())
			},
			wantState: settlement.StateAwardingRunnerUp,
			after: func(t *testing.T, s *settlement.Settlement) {
				assert.Equal(t, s.RunnerUp(), s.Winner(), "the runner-up is the debtor now")
				assert.Equal(t, 2, s.Attempt())
			},
		},
		{
			name:    "runner-up awarded fast-forwards from awaiting payment",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{qualifies: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				// WinnerReassignedV1 overtook our own AwardingRunnerUp
				// commit (§6.3) — the event is the proof.
				return s.RunnerUpAwarded(s.RunnerUp(), s.RunnerUpAmount())
			},
			wantState: settlement.StateAwardingRunnerUp,
			after: func(t *testing.T, s *settlement.Settlement) {
				assert.Equal(t, 2, s.Attempt())
			},
		},
		{
			name:    "runner-up awarded with a foreign bidder is a mismatch anomaly",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{qualifies: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.RunnerUpAwarded(newBidderID(t), s.RunnerUpAmount())
			},
			wantErr: settlement.ErrWinnerMismatch,
		},
		{
			name:    "runner-up awarded with a wrong price is a mismatch anomaly",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{qualifies: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.RunnerUpAwarded(s.RunnerUp(), money(t, 1))
			},
			wantErr: settlement.ErrWinnerMismatch,
		},
		{
			name: "second chance invoice moves awarding to second chance payment",
			arrange: func(t *testing.T) *settlement.Settlement {
				s := awaitingSaga(t, closingOpts{qualifies: true})
				require.NoError(t, s.ApplyNextStep(settlement.StepAwardRunnerUp, noReason))
				require.NoError(t, s.RunnerUpAwarded(s.RunnerUp(), s.RunnerUpAmount()))
				return s
			},
			act: func(t *testing.T, s *settlement.Settlement) error {
				inv2 := newInvoiceID(t)
				if err := s.SecondChanceInvoiceIssued(inv2); err != nil {
					return err
				}
				assert.Equal(t, inv2, s.InvoiceID(), "the saga awaits the attempt-2 invoice now")
				return nil
			},
			wantState: settlement.StateSecondChancePayment,
		},
		{
			name:    "second chance invoice outside awarding is an acked duplicate",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.SecondChanceInvoiceIssued(newInvoiceID(t))
			},
			wantErr: settlement.ErrUnexpectedTransition,
		},
		{
			name: "payment received settles the second chance",
			arrange: func(t *testing.T) *settlement.Settlement {
				s, _ := secondChanceSaga(t, closingOpts{})
				return s
			},
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.PaymentReceived(s.InvoiceID())
			},
			wantState: settlement.StateSettled,
		},
		{
			name:    "relist step concludes awaiting payment with the reason",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{noRunnerUp: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.ApplyNextStep(settlement.StepRelist, settlement.ReasonPaymentTimeout)
			},
			wantState: settlement.StateRelisted,
			after: func(t *testing.T, s *settlement.Settlement) {
				assert.Equal(t, settlement.ReasonPaymentTimeout, s.FailureReason())
			},
		},
		{
			name: "fail-unsold step concludes the second chance with the decline reason",
			arrange: func(t *testing.T) *settlement.Settlement {
				s, _ := secondChanceSaga(t, closingOpts{relistGen: 1})
				return s
			},
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.ApplyNextStep(settlement.StepFailUnsold, settlement.ReasonSecondChanceDeclined)
			},
			wantState: settlement.StateFailedUnsold,
			after: func(t *testing.T, s *settlement.Settlement) {
				assert.Equal(t, settlement.ReasonSecondChanceDeclined, s.FailureReason())
			},
		},
		{
			name:    "relist step without a reason is refused",
			arrange: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{noRunnerUp: true}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				err := s.ApplyNextStep(settlement.StepRelist, noReason)
				assert.Error(t, err)
				assert.NotErrorIs(t, err, settlement.ErrUnexpectedTransition)
				return nil
			},
			wantState: settlement.StateAwaitingPayment,
		},
		{
			name:    "compensation at a terminal state is an acked duplicate",
			arrange: func(t *testing.T) *settlement.Settlement { return settledSaga(t, closingOpts{}) },
			act: func(t *testing.T, s *settlement.Settlement) error {
				return s.ApplyNextStep(settlement.StepFailUnsold, settlement.ReasonPaymentTimeout)
			},
			wantErr: settlement.ErrUnexpectedTransition,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.arrange(t)
			err := tc.act(t, s)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantState, s.State())
			if tc.after != nil {
				tc.after(t, s)
			}
		})
	}
}

// TestDecidePhase covers the pure decision functions, including the
// compensation fork and both fast-forward seams (§6.3).
func TestDecidePhase(t *testing.T) {
	t.Parallel()

	t.Run("invoice issue is due only at started", func(t *testing.T) {
		t.Parallel()
		started := startedSaga(t, closingOpts{})
		due, err := started.DecideOnInvoiceIssue()
		require.NoError(t, err)
		assert.True(t, due)

		awaiting := awaitingSaga(t, closingOpts{})
		_, err = awaiting.DecideOnInvoiceIssue()
		assert.ErrorIs(t, err, settlement.ErrUnexpectedTransition)
	})

	t.Run("payment timeout fork", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			saga func(t *testing.T) *settlement.Settlement
			want settlement.NextStep
		}{
			{
				name: "qualifying runner-up on attempt one gets the second chance",
				saga: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{qualifies: true}) },
				want: settlement.StepAwardRunnerUp,
			},
			{
				name: "non-qualifying runner-up means relist",
				saga: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{}) },
				want: settlement.StepRelist,
			},
			{
				name: "no runner-up and no relist yet means relist",
				saga: func(t *testing.T) *settlement.Settlement { return awaitingSaga(t, closingOpts{noRunnerUp: true}) },
				want: settlement.StepRelist,
			},
			{
				name: "relist cap reached means failed unsold",
				saga: func(t *testing.T) *settlement.Settlement {
					return awaitingSaga(t, closingOpts{noRunnerUp: true, relistGen: 1})
				},
				want: settlement.StepFailUnsold,
			},
			{
				name: "second chance timeout never awards again",
				saga: func(t *testing.T) *settlement.Settlement {
					s, _ := secondChanceSaga(t, closingOpts{})
					return s
				},
				want: settlement.StepRelist,
			},
			{
				name: "second chance timeout of a relisted generation fails unsold",
				saga: func(t *testing.T) *settlement.Settlement {
					s, _ := secondChanceSaga(t, closingOpts{relistGen: 1})
					return s
				},
				want: settlement.StepFailUnsold,
			},
			{
				name: "timeout at started fast-forwards through the same fork",
				saga: func(t *testing.T) *settlement.Settlement { return startedSaga(t, closingOpts{qualifies: true}) },
				want: settlement.StepAwardRunnerUp,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				s := tc.saga(t)
				step, err := s.DecideOnPaymentTimeout(s.InvoiceID())
				require.NoError(t, err)
				assert.Equal(t, tc.want, step)
			})
		}
	})

	t.Run("stale attempt-1 expiry at second chance is an acked duplicate", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		_, err := s.DecideOnPaymentTimeout(newInvoiceID(t))
		assert.ErrorIs(t, err, settlement.ErrUnexpectedTransition)
	})

	t.Run("timeout while awarding the runner-up is an acked duplicate", func(t *testing.T) {
		t.Parallel()
		s := awaitingSaga(t, closingOpts{qualifies: true})
		require.NoError(t, s.ApplyNextStep(settlement.StepAwardRunnerUp, settlement.FailureReason{}))
		_, err := s.DecideOnPaymentTimeout(s.InvoiceID())
		assert.ErrorIs(t, err, settlement.ErrUnexpectedTransition)
	})

	t.Run("winner reassigned is admitted while awaiting or awarding", func(t *testing.T) {
		t.Parallel()
		awaiting := awaitingSaga(t, closingOpts{qualifies: true})
		assert.NoError(t, awaiting.DecideOnWinnerReassigned(awaiting.RunnerUp(), awaiting.RunnerUpAmount()))

		require.NoError(t, awaiting.ApplyNextStep(settlement.StepAwardRunnerUp, settlement.FailureReason{}))
		assert.NoError(t, awaiting.DecideOnWinnerReassigned(awaiting.RunnerUp(), awaiting.RunnerUpAmount()))
	})

	t.Run("winner reassigned past the award is an acked duplicate", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		err := s.DecideOnWinnerReassigned(s.RunnerUp(), s.RunnerUpAmount())
		assert.ErrorIs(t, err, settlement.ErrUnexpectedTransition)
	})

	t.Run("winner reassigned to a stranger is a mismatch anomaly", func(t *testing.T) {
		t.Parallel()
		s := awaitingSaga(t, closingOpts{qualifies: true})
		err := s.DecideOnWinnerReassigned(newBidderID(t), s.RunnerUpAmount())
		assert.ErrorIs(t, err, settlement.ErrWinnerMismatch)
	})
}

func TestUnmarshalFromDatabase(t *testing.T) {
	t.Parallel()

	t.Run("roundtrips a rehydrated saga", func(t *testing.T) {
		t.Parallel()
		auctionID, winner := newAuctionID(t), newBidderID(t)
		inv := newInvoiceID(t)
		s, err := settlement.UnmarshalFromDatabase(
			auctionID, settlement.StateRelisted, winner, money(t, 100),
			settlement.BidderID{}, settlement.Money{}, false,
			0, 1, inv, settlement.ReasonPaymentTimeout, 7,
		)
		require.NoError(t, err)
		assert.Equal(t, auctionID, s.AuctionID())
		assert.Equal(t, settlement.StateRelisted, s.State())
		assert.Equal(t, settlement.ReasonPaymentTimeout, s.FailureReason())
		assert.Equal(t, int64(7), s.Version())
	})

	t.Run("rejects invalid storage rows", func(t *testing.T) {
		t.Parallel()
		_, err := settlement.UnmarshalFromDatabase(
			settlement.AuctionID{}, settlement.StateStarted, newBidderID(t), money(t, 100),
			settlement.BidderID{}, settlement.Money{}, false, 0, 1, newInvoiceID(t), settlement.FailureReason{}, 1,
		)
		assert.Error(t, err, "auction id is required")

		_, err = settlement.UnmarshalFromDatabase(
			newAuctionID(t), settlement.StateStarted, newBidderID(t), money(t, 100),
			settlement.BidderID{}, settlement.Money{}, false, 0, 3, newInvoiceID(t), settlement.FailureReason{}, 1,
		)
		assert.Error(t, err, "attempt must be 1 or 2")
	})
}
