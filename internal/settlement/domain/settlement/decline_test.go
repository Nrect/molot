package settlement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/domain/settlement"
)

func TestCanRunnerUpDecline(t *testing.T) {
	t.Parallel()

	t.Run("the runner-up may decline the live offer", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		assert.NoError(t, settlement.CanRunnerUpDecline(s.RunnerUp(), *s))
	})

	t.Run("a stranger is not the offer recipient", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		assert.ErrorIs(t, settlement.CanRunnerUpDecline(newBidderID(t), *s), settlement.ErrNotOfferRecipient)
	})

	t.Run("there is no offer before the second chance", func(t *testing.T) {
		t.Parallel()
		s := awaitingSaga(t, closingOpts{qualifies: true})
		assert.ErrorIs(t, settlement.CanRunnerUpDecline(s.RunnerUp(), *s), settlement.ErrNotOfferRecipient)
	})

	t.Run("a zero actor is never the recipient", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		assert.ErrorIs(t, settlement.CanRunnerUpDecline(settlement.BidderID{}, *s), settlement.ErrNotOfferRecipient)
	})
}

func TestDecideOnDecline(t *testing.T) {
	t.Parallel()

	t.Run("decline forks to relist while the cap allows", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		step, err := s.DecideOnDecline(s.RunnerUp())
		require.NoError(t, err)
		assert.Equal(t, settlement.StepRelist, step)
	})

	t.Run("decline of a relisted generation fails unsold", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{relistGen: 1})
		step, err := s.DecideOnDecline(s.RunnerUp())
		require.NoError(t, err)
		assert.Equal(t, settlement.StepFailUnsold, step)
	})

	t.Run("a stranger gets not-offer-recipient", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		_, err := s.DecideOnDecline(newBidderID(t))
		assert.ErrorIs(t, err, settlement.ErrNotOfferRecipient)
	})

	t.Run("decline after the payment won is offer-already-paid", func(t *testing.T) {
		t.Parallel()
		s, inv2 := secondChanceSaga(t, closingOpts{})
		require.NoError(t, s.PaymentReceived(inv2))
		_, err := s.DecideOnDecline(s.RunnerUp())
		assert.ErrorIs(t, err, settlement.ErrOfferAlreadyPaid)
	})

	t.Run("decline retried after the same outcome is an acked duplicate", func(t *testing.T) {
		t.Parallel()
		s, _ := secondChanceSaga(t, closingOpts{})
		require.NoError(t, s.ApplyNextStep(settlement.StepRelist, settlement.ReasonSecondChanceDeclined))
		_, err := s.DecideOnDecline(s.RunnerUp())
		assert.ErrorIs(t, err, settlement.ErrUnexpectedTransition,
			"the benign race against expiry resolves to the idempotent 204 (§6.4)")
	})

	t.Run("the runner-up before the offer exists gets not-offer-recipient", func(t *testing.T) {
		t.Parallel()
		s := awaitingSaga(t, closingOpts{qualifies: true})
		_, err := s.DecideOnDecline(s.RunnerUp())
		assert.ErrorIs(t, err, settlement.ErrNotOfferRecipient)
	})
}
