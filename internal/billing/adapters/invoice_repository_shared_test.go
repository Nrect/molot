package adapters_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/domain/invoice"
)

// testInvoiceRepository is the ONE shared suite run against every
// invoice.Repository implementation (BOOK_AUDIT rule 42): unique data
// per subtest, t.Parallel(), assertions by concrete id, no cleanup.
func testInvoiceRepository(t *testing.T, newRepo func(t *testing.T) invoice.Repository) {
	t.Helper()

	t.Run("add and get roundtrip", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})

		require.NoError(t, repo.Add(context.Background(), inv))

		got, err := repo.Get(context.Background(), inv.ID(), inv.Debtor())
		require.NoError(t, err)
		assert.Equal(t, inv.ID(), got.ID())
		assert.Equal(t, inv.AuctionID(), got.AuctionID())
		assert.Equal(t, inv.Debtor(), got.Debtor())
		assert.Equal(t, inv.Hammer(), got.Hammer())
		assert.Equal(t, inv.Commission(), got.Commission())
		assert.Equal(t, inv.Total(), got.Total())
		assert.Equal(t, invoice.StatusPending, got.Status())
		assert.Equal(t, inv.Attempt(), got.Attempt())
		assert.True(t, got.PSPRef().IsZero())
		assert.WithinDuration(t, inv.DueAt(), got.DueAt(), time.Millisecond)
	})

	t.Run("add same id again reports already issued", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), inv))

		dup := issueFixture(t, fixtureOpts{id: inv.ID(), auctionID: inv.AuctionID(), debtor: inv.Debtor()})
		assert.ErrorIs(t, repo.Add(context.Background(), dup), invoice.ErrInvoiceAlreadyIssued)
	})

	t.Run("add same auction and attempt reports already issued", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), inv))

		// Different deterministic id would be impossible in production,
		// but UNIQUE(auction_id, attempt) is the second line of defense.
		dup := issueFixture(t, fixtureOpts{auctionID: inv.AuctionID(), debtor: inv.Debtor()})
		assert.ErrorIs(t, repo.Add(context.Background(), dup), invoice.ErrInvoiceAlreadyIssued)
	})

	t.Run("get by a foreign actor is not found", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), inv))

		stranger := newBidderID(t)
		_, err := repo.Get(context.Background(), inv.ID(), stranger)
		var notFound invoice.NotFoundError
		require.ErrorAs(t, err, &notFound, "foreign must be indistinguishable from missing")
		assert.Equal(t, inv.ID(), notFound.InvoiceID)
	})

	t.Run("get missing invoice is not found", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		id, err := invoice.NewInvoiceID(uuid.New())
		require.NoError(t, err)

		_, getErr := repo.Get(context.Background(), id, newBidderID(t))
		var notFound invoice.NotFoundError
		assert.ErrorAs(t, getErr, &notFound)
	})

	t.Run("update persists the transition", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), inv))
		ref, err := invoice.NewPaymentReference("psp-" + uuid.NewString())
		require.NoError(t, err)

		err = repo.Update(context.Background(), inv.ID(), inv.Debtor(),
			func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
				if err := i.MarkPaid(ref, time.Now()); err != nil {
					return nil, err
				}
				return i, nil
			})
		require.NoError(t, err)

		got, err := repo.Get(context.Background(), inv.ID(), inv.Debtor())
		require.NoError(t, err)
		assert.Equal(t, invoice.StatusPaid, got.Status())
		assert.Equal(t, ref, got.PSPRef())
	})

	t.Run("update by a foreign actor is not found and does not mutate", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), inv))
		stranger := newBidderID(t)

		fnCalled := false
		err := repo.Update(context.Background(), inv.ID(), stranger,
			func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
				fnCalled = true
				return i, nil
			})
		var notFound invoice.NotFoundError
		require.ErrorAs(t, err, &notFound, "anti-enumeration: forbidden maps to not-found")
		assert.False(t, fnCalled, "updateFn must not run for a foreign actor")

		got, err := repo.Get(context.Background(), inv.ID(), inv.Debtor())
		require.NoError(t, err)
		assert.Equal(t, invoice.StatusPending, got.Status())
	})

	t.Run("update as system needs no actor", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{duePast: true})
		require.NoError(t, repo.Add(context.Background(), inv))

		err := repo.UpdateAsSystem(context.Background(), inv.ID(),
			func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
				if err := i.Expire(time.Now()); err != nil {
					return nil, err
				}
				return i, nil
			})
		require.NoError(t, err)

		got, err := repo.Get(context.Background(), inv.ID(), inv.Debtor())
		require.NoError(t, err)
		assert.Equal(t, invoice.StatusExpired, got.Status())
	})

	t.Run("update rolls back when updateFn fails", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), inv))
		sentinel := errors.New("business rule refused")

		err := repo.Update(context.Background(), inv.ID(), inv.Debtor(),
			func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
				// Mutate, then fail: nothing of this may persist.
				ref, refErr := invoice.NewPaymentReference("psp-rollback")
				if refErr != nil {
					return nil, refErr
				}
				if mpErr := i.MarkPaid(ref, time.Now()); mpErr != nil {
					return nil, mpErr
				}
				return nil, sentinel
			})
		require.ErrorIs(t, err, sentinel, "updateFn error must be returned as-is")

		got, getErr := repo.Get(context.Background(), inv.ID(), inv.Debtor())
		require.NoError(t, getErr)
		assert.Equal(t, invoice.StatusPending, got.Status(), "rollback must discard the mutation")
		assert.True(t, got.PSPRef().IsZero())
	})

	t.Run("pending due before returns only due pending invoices", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		now := time.Now()

		due := issueFixture(t, fixtureOpts{duePast: true})
		require.NoError(t, repo.Add(context.Background(), due))
		notDue := issueFixture(t, fixtureOpts{})
		require.NoError(t, repo.Add(context.Background(), notDue))
		paid := issueFixture(t, fixtureOpts{duePast: true})
		require.NoError(t, repo.Add(context.Background(), paid))
		ref, err := invoice.NewPaymentReference("psp-" + uuid.NewString())
		require.NoError(t, err)
		require.NoError(t, repo.Update(context.Background(), paid.ID(), paid.Debtor(),
			func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
				if err := i.MarkPaid(ref, now); err != nil {
					return nil, err
				}
				return i, nil
			}))

		ids, err := repo.PendingDueBefore(context.Background(), now, 1000)
		require.NoError(t, err)

		// The store is shared across parallel subtests: assert by
		// concrete id (rule 42), not by exact slice contents.
		assert.Contains(t, ids, due.ID())
		assert.NotContains(t, ids, notDue.ID(), "future due date must not be scanned")
		assert.NotContains(t, ids, paid.ID(), "paid invoices are out of the partial scan")
	})

	t.Run("paid vs expired race has exactly one winner", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		inv := issueFixture(t, fixtureOpts{duePast: true})
		require.NoError(t, repo.Add(context.Background(), inv))
		ref, err := invoice.NewPaymentReference("psp-" + uuid.NewString())
		require.NoError(t, err)

		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			results <- repo.Update(context.Background(), inv.ID(), inv.Debtor(),
				func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
					if err := i.MarkPaid(ref, time.Now()); err != nil {
						return nil, err
					}
					return i, nil
				})
		}()
		go func() {
			defer wg.Done()
			<-start
			results <- repo.UpdateAsSystem(context.Background(), inv.ID(),
				func(_ context.Context, i *invoice.Invoice) (*invoice.Invoice, error) {
					if err := i.Expire(time.Now()); err != nil {
						return nil, err
					}
					return i, nil
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
			assert.True(t,
				errors.Is(err, invoice.ErrInvoiceAlreadyPaid) || errors.Is(err, invoice.ErrInvoiceExpired),
				"the loser must see a guard sentinel, got: %v", err)
		}
		assert.Equal(t, 1, wins, "exactly one transition may win — the saga never sees both signals")
	})
}

// --- fixtures ----------------------------------------------------------------

type fixtureOpts struct {
	id        invoice.InvoiceID // zero → random
	auctionID invoice.AuctionID // zero → random
	debtor    invoice.BidderID  // zero → random
	duePast   bool              // issue 72h ago (48h term) so expiry is legal
}

func issueFixture(t *testing.T, opts fixtureOpts) *invoice.Invoice {
	t.Helper()
	if opts.id.IsZero() {
		var err error
		opts.id, err = invoice.NewInvoiceID(uuid.New())
		require.NoError(t, err)
	}
	if opts.auctionID.IsZero() {
		var err error
		opts.auctionID, err = invoice.NewAuctionID(uuid.New())
		require.NoError(t, err)
	}
	if opts.debtor.IsZero() {
		opts.debtor = newBidderID(t)
	}
	cur, err := invoice.NewCurrency("USD")
	require.NoError(t, err)
	hammer, err := invoice.NewMoney(100_000, cur)
	require.NoError(t, err)
	policy, err := invoice.NewCommissionPolicy(1_000)
	require.NoError(t, err)
	term, err := invoice.NewPaymentTerm(48 * time.Hour)
	require.NoError(t, err)
	issuedAt := time.Now()
	if opts.duePast {
		issuedAt = issuedAt.Add(-72 * time.Hour)
	}
	inv, err := invoice.NewInvoice(
		opts.id, opts.auctionID, opts.debtor, hammer,
		policy, term, invoice.AttemptFirst, issuedAt,
	)
	require.NoError(t, err)
	return inv
}

func newBidderID(t *testing.T) invoice.BidderID {
	t.Helper()
	b, err := invoice.NewBidderID(uuid.New())
	require.NoError(t, err)
	return b
}
