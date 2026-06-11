package adapters_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/domain/settlement"
)

// testSettlementRepository is the ONE shared suite run against every
// settlement.Repository implementation (rule 42): unique data per
// subtest, t.Parallel(), assertions by concrete id, no cleanup.
func testSettlementRepository(t *testing.T, newRepo func(t *testing.T) settlement.Repository) {
	t.Helper()
	ctx := context.Background()

	t.Run("add and get roundtrip", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{qualifies: true})

		require.NoError(t, repo.Add(ctx, s))

		got, err := repo.Get(ctx, s.AuctionID())
		require.NoError(t, err)
		assert.Equal(t, s.AuctionID(), got.AuctionID())
		assert.Equal(t, settlement.StateStarted, got.State())
		assert.Equal(t, s.Winner(), got.Winner())
		assert.Equal(t, s.Hammer(), got.Hammer())
		assert.Equal(t, s.RunnerUp(), got.RunnerUp())
		assert.Equal(t, s.RunnerUpAmount(), got.RunnerUpAmount())
		assert.True(t, got.RunnerUpQualifies())
		assert.Equal(t, s.RelistGeneration(), got.RelistGeneration())
		assert.Equal(t, 1, got.Attempt())
		assert.Equal(t, s.InvoiceID(), got.InvoiceID())
		assert.True(t, got.FailureReason().IsZero())
		assert.Equal(t, int64(1), got.Version())
	})

	t.Run("add and get roundtrip without a runner-up", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{noRunnerUp: true})

		require.NoError(t, repo.Add(ctx, s))

		got, err := repo.Get(ctx, s.AuctionID())
		require.NoError(t, err)
		assert.True(t, got.RunnerUp().IsZero())
		assert.True(t, got.RunnerUpAmount().IsZero())
		assert.False(t, got.RunnerUpQualifies())
	})

	t.Run("duplicate add is a silent no-op preserving the stored state", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{})
		require.NoError(t, repo.Add(ctx, s))
		require.NoError(t, repo.Update(ctx, s.AuctionID(),
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				if err := current.InvoiceIssued(current.InvoiceID()); err != nil {
					return nil, err
				}
				return current, nil
			}))

		// The redelivered AuctionClosedV1 re-inserts: ON CONFLICT DO NOTHING.
		fresh := startedFixture(t, sagaOpts{})
		duplicate, err := settlement.UnmarshalFromDatabase(
			s.AuctionID(), settlement.StateStarted, fresh.Winner(), fresh.Hammer(),
			settlement.BidderID{}, settlement.Money{}, false, 0, 1,
			fresh.InvoiceID(), settlement.FailureReason{}, 1,
		)
		require.NoError(t, err)
		require.NoError(t, repo.Add(ctx, duplicate))

		got, err := repo.Get(ctx, s.AuctionID())
		require.NoError(t, err)
		assert.Equal(t, settlement.StateAwaitingPayment, got.State(), "the advanced state must survive the duplicate insert")
		assert.Equal(t, s.Winner(), got.Winner())
	})

	t.Run("get missing settlement is not found", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		id, err := settlement.NewAuctionID(uuid.New())
		require.NoError(t, err)

		_, getErr := repo.Get(ctx, id)
		var notFound settlement.NotFoundError
		require.ErrorAs(t, getErr, &notFound)
		assert.Equal(t, id, notFound.AuctionID)
	})

	t.Run("update persists the transition and bumps the version", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{})
		require.NoError(t, repo.Add(ctx, s))

		require.NoError(t, repo.Update(ctx, s.AuctionID(),
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				if err := current.InvoiceIssued(current.InvoiceID()); err != nil {
					return nil, err
				}
				return current, nil
			}))

		got, err := repo.Get(ctx, s.AuctionID())
		require.NoError(t, err)
		assert.Equal(t, settlement.StateAwaitingPayment, got.State())
		assert.Equal(t, int64(2), got.Version())
	})

	t.Run("terminal transition persists the failure reason", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{noRunnerUp: true})
		require.NoError(t, repo.Add(ctx, s))

		require.NoError(t, repo.Update(ctx, s.AuctionID(),
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				if err := current.InvoiceIssued(current.InvoiceID()); err != nil {
					return nil, err
				}
				if err := current.ApplyNextStep(settlement.StepRelist, settlement.ReasonPaymentTimeout); err != nil {
					return nil, err
				}
				return current, nil
			}))

		got, err := repo.Get(ctx, s.AuctionID())
		require.NoError(t, err)
		assert.Equal(t, settlement.StateRelisted, got.State())
		assert.Equal(t, settlement.ReasonPaymentTimeout, got.FailureReason())
	})

	t.Run("update rolls back when updateFn fails", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{})
		require.NoError(t, repo.Add(ctx, s))
		sentinel := errors.New("business rule refused")

		err := repo.Update(ctx, s.AuctionID(),
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				// Mutate, then fail: nothing of this may persist.
				if err := current.InvoiceIssued(current.InvoiceID()); err != nil {
					return nil, err
				}
				return nil, sentinel
			})
		require.ErrorIs(t, err, sentinel, "updateFn error must be returned as-is")

		got, getErr := repo.Get(ctx, s.AuctionID())
		require.NoError(t, getErr)
		assert.Equal(t, settlement.StateStarted, got.State(), "rollback must discard the mutation")
		assert.Equal(t, int64(1), got.Version())
	})

	t.Run("update of a missing settlement is not found", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		id, err := settlement.NewAuctionID(uuid.New())
		require.NoError(t, err)

		updateErr := repo.Update(ctx, id,
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				return current, nil
			})
		var notFound settlement.NotFoundError
		assert.ErrorAs(t, updateErr, &notFound)
	})

	t.Run("paid vs timeout race has exactly one winner", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		s := startedFixture(t, sagaOpts{noRunnerUp: true, relistGen: 1})
		require.NoError(t, repo.Add(ctx, s))
		require.NoError(t, repo.Update(ctx, s.AuctionID(),
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				if err := current.InvoiceIssued(current.InvoiceID()); err != nil {
					return nil, err
				}
				return current, nil
			}))

		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			results <- repo.Update(ctx, s.AuctionID(),
				func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
					if err := current.PaymentReceived(current.InvoiceID()); err != nil {
						return nil, err
					}
					return current, nil
				})
		}()
		go func() {
			defer wg.Done()
			<-start
			results <- repo.Update(ctx, s.AuctionID(),
				func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
					if err := current.ApplyNextStep(settlement.StepFailUnsold, settlement.ReasonPaymentTimeout); err != nil {
						return nil, err
					}
					return current, nil
				})
		}()

		close(start)
		wg.Wait()
		close(results)

		var wins int
		for err := range results {
			if err == nil {
				wins++
				continue
			}
			assert.ErrorIs(t, err, settlement.ErrUnexpectedTransition,
				"the loser must see the state-machine guard, got: %v", err)
		}
		assert.Equal(t, 1, wins, "exactly one commit may win — the duplicate acks (§6.3)")
	})
}

// --- fixtures ------------------------------------------------------------------

type sagaOpts struct {
	noRunnerUp bool
	qualifies  bool
	relistGen  int
}

func startedFixture(t *testing.T, opts sagaOpts) *settlement.Settlement {
	t.Helper()
	auctionID, err := settlement.NewAuctionID(uuid.New())
	require.NoError(t, err)
	winner, err := settlement.NewBidderID(uuid.New())
	require.NoError(t, err)
	cur, err := settlement.NewCurrency("EUR")
	require.NoError(t, err)
	hammer, err := settlement.NewMoney(100_000, cur)
	require.NoError(t, err)
	firstInvoice, err := settlement.NewInvoiceID(uuid.New())
	require.NoError(t, err)

	var runnerUp settlement.BidderID
	var runnerUpAmount settlement.Money
	if !opts.noRunnerUp {
		runnerUp, err = settlement.NewBidderID(uuid.New())
		require.NoError(t, err)
		runnerUpAmount, err = settlement.NewMoney(90_000, cur)
		require.NoError(t, err)
	}

	closing, err := settlement.NewClosing(
		auctionID, winner, hammer, runnerUp, runnerUpAmount,
		opts.qualifies && !opts.noRunnerUp, opts.relistGen, firstInvoice,
	)
	require.NoError(t, err)
	s, err := settlement.Start(closing)
	require.NoError(t, err)
	return s
}
