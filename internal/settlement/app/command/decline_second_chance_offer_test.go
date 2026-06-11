package command_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/errs"
	"molot/internal/settlement/app/command"
	"molot/internal/settlement/domain/settlement"
)

// --- recording spies (rule 41) ----------------------------------------------

type declineRepoSpy struct {
	mu        sync.Mutex
	byAuction map[settlement.AuctionID]settlement.Settlement

	updateErr error  // scripted commit failure
	onUpdate  func() // runs under the lock before updateFn
}

func newDeclineRepoSpy() *declineRepoSpy {
	return &declineRepoSpy{byAuction: map[settlement.AuctionID]settlement.Settlement{}}
}

func (r *declineRepoSpy) Add(_ context.Context, s *settlement.Settlement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byAuction[s.AuctionID()]; ok {
		return nil
	}
	r.byAuction[s.AuctionID()] = *s
	return nil
}

func (r *declineRepoSpy) Get(_ context.Context, id settlement.AuctionID) (*settlement.Settlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.byAuction[id]
	if !ok {
		return nil, settlement.NotFoundError{AuctionID: id}
	}
	cp := stored
	return &cp, nil
}

func (r *declineRepoSpy) Update(
	ctx context.Context,
	id settlement.AuctionID,
	updateFn func(ctx context.Context, s *settlement.Settlement) (*settlement.Settlement, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	if r.onUpdate != nil {
		hook := r.onUpdate
		r.onUpdate = nil
		hook()
	}
	stored, ok := r.byAuction[id]
	if !ok {
		return settlement.NotFoundError{AuctionID: id}
	}
	work := stored
	updated, err := updateFn(ctx, &work)
	if err != nil {
		return err
	}
	r.byAuction[id] = *updated
	return nil
}

func (r *declineRepoSpy) seed(s *settlement.Settlement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byAuction[s.AuctionID()] = *s
}

func (r *declineRepoSpy) mutateLocked(t *testing.T, id settlement.AuctionID, fn func(s *settlement.Settlement) error) {
	t.Helper()
	stored, ok := r.byAuction[id]
	require.True(t, ok)
	require.NoError(t, fn(&stored))
	r.byAuction[id] = stored
}

func (r *declineRepoSpy) mustGet(t *testing.T, id settlement.AuctionID) settlement.Settlement {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.byAuction[id]
	require.True(t, ok)
	return stored
}

type declineAuctionSpy struct {
	markFails []settlement.FailureReason
	relists   []struct {
		originalID, newID settlement.AuctionID
		startsAt, endsAt  time.Time
	}
	markFailErr, relistErr error
	onMarkSaleFailed       func() // race hook between effect and commit
}

func (g *declineAuctionSpy) MarkSaleFailed(_ context.Context, _ settlement.AuctionID, reason settlement.FailureReason) error {
	g.markFails = append(g.markFails, reason)
	if g.onMarkSaleFailed != nil {
		hook := g.onMarkSaleFailed
		g.onMarkSaleFailed = nil
		hook()
	}
	return g.markFailErr
}

func (g *declineAuctionSpy) Relist(_ context.Context, originalID, newID settlement.AuctionID, startsAt, endsAt time.Time) error {
	g.relists = append(g.relists, struct {
		originalID, newID settlement.AuctionID
		startsAt, endsAt  time.Time
	}{originalID, newID, startsAt, endsAt})
	return g.relistErr
}

type declineBillingSpy struct {
	voids   []settlement.InvoiceID
	voidErr error
}

func (g *declineBillingSpy) VoidInvoice(_ context.Context, invoiceID settlement.InvoiceID) error {
	g.voids = append(g.voids, invoiceID)
	return g.voidErr
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// --- fixtures ----------------------------------------------------------------

var testNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type declineFixture struct {
	repo     *declineRepoSpy
	auctions *declineAuctionSpy
	billing  *declineBillingSpy
	handler  command.DeclineSecondChanceOfferHandler
}

func newDeclineFixture(t *testing.T) *declineFixture {
	t.Helper()
	relist, err := settlement.NewRelistPolicy(time.Hour, 24*time.Hour)
	require.NoError(t, err)
	repo := newDeclineRepoSpy()
	auctions := &declineAuctionSpy{}
	billing := &declineBillingSpy{}
	return &declineFixture{
		repo:     repo,
		auctions: auctions,
		billing:  billing,
		handler:  command.NewDeclineSecondChanceOfferHandler(repo, auctions, billing, relist, fixedClock{now: testNow}),
	}
}

// secondChanceSaga builds (through the domain API) a saga awaiting the
// second-chance payment and seeds it into the repo.
func secondChanceSaga(t *testing.T, f *declineFixture, relistGen int) *settlement.Settlement {
	t.Helper()
	auctionID, err := settlement.NewAuctionID(uuid.New())
	require.NoError(t, err)
	winner, err := settlement.NewBidderID(uuid.New())
	require.NoError(t, err)
	runnerUp, err := settlement.NewBidderID(uuid.New())
	require.NoError(t, err)
	cur, err := settlement.NewCurrency("EUR")
	require.NoError(t, err)
	hammer, err := settlement.NewMoney(100_000, cur)
	require.NoError(t, err)
	runnerUpAmount, err := settlement.NewMoney(90_000, cur)
	require.NoError(t, err)

	closing, err := settlement.NewClosing(
		auctionID, winner, hammer, runnerUp, runnerUpAmount, true, relistGen,
		command.InvoiceIDForAttempt(auctionID, 1),
	)
	require.NoError(t, err)
	s, err := settlement.Start(closing)
	require.NoError(t, err)
	require.NoError(t, s.InvoiceIssued(s.InvoiceID()))
	require.NoError(t, s.ApplyNextStep(settlement.StepAwardRunnerUp, settlement.FailureReason{}))
	require.NoError(t, s.RunnerUpAwarded(runnerUp, runnerUpAmount))
	require.NoError(t, s.SecondChanceInvoiceIssued(command.InvoiceIDForAttempt(auctionID, 2)))

	f.repo.seed(s)
	return s
}

func declineCmd(s *settlement.Settlement) command.DeclineSecondChanceOffer {
	return command.DeclineSecondChanceOffer{AuctionID: s.AuctionID(), Actor: s.RunnerUp()}
}

// --- tests ---------------------------------------------------------------------

func TestDeclineSecondChanceOffer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("decline voids the invoice, fails the sale and relists", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)

		require.NoError(t, f.handler.Handle(ctx, declineCmd(s)))

		assert.Equal(t, []settlement.InvoiceID{s.InvoiceID()}, f.billing.voids)
		require.Len(t, f.auctions.markFails, 1)
		assert.Equal(t, settlement.ReasonSecondChanceDeclined, f.auctions.markFails[0])
		require.Len(t, f.auctions.relists, 1)
		assert.Equal(t, command.RelistIDFor(s.AuctionID()), f.auctions.relists[0].newID)
		assert.Equal(t, testNow.Add(time.Hour), f.auctions.relists[0].startsAt)

		final := f.repo.mustGet(t, s.AuctionID())
		assert.Equal(t, settlement.StateRelisted, final.State())
		assert.Equal(t, settlement.ReasonSecondChanceDeclined, final.FailureReason())
	})

	t.Run("decline past the relist cap fails unsold", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 1)

		require.NoError(t, f.handler.Handle(ctx, declineCmd(s)))

		assert.Empty(t, f.auctions.relists)
		final := f.repo.mustGet(t, s.AuctionID())
		assert.Equal(t, settlement.StateFailedUnsold, final.State())
		assert.Equal(t, settlement.ReasonSecondChanceDeclined, final.FailureReason())
	})

	t.Run("a stranger gets 403 not-offer-recipient and nothing happens", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)
		stranger, err := settlement.NewBidderID(uuid.New())
		require.NoError(t, err)

		handleErr := f.handler.Handle(ctx, command.DeclineSecondChanceOffer{AuctionID: s.AuctionID(), Actor: stranger})

		assert.ErrorIs(t, handleErr, errs.NewForbiddenError("not-offer-recipient"))
		assert.Empty(t, f.billing.voids)
		assert.Equal(t, settlement.StateSecondChancePayment, f.repo.mustGet(t, s.AuctionID()).State())
	})

	t.Run("an unknown settlement is 404", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		auctionID, err := settlement.NewAuctionID(uuid.New())
		require.NoError(t, err)
		actor, err := settlement.NewBidderID(uuid.New())
		require.NoError(t, err)

		handleErr := f.handler.Handle(ctx, command.DeclineSecondChanceOffer{AuctionID: auctionID, Actor: actor})
		assert.ErrorIs(t, handleErr, errs.NewNotFoundError("settlement-not-found"))
	})

	t.Run("a paid offer wins: 409 offer-already-paid, the saga untouched", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)
		// The payment landed between our decide and the void: billing
		// refuses (the adapter translates to the domain sentinel).
		f.billing.voidErr = settlement.ErrOfferAlreadyPaid

		handleErr := f.handler.Handle(ctx, declineCmd(s))

		assert.ErrorIs(t, handleErr, errs.NewConflictError("offer-already-paid"))
		assert.Empty(t, f.auctions.markFails, "the compensation must not run — the sale settles")
		assert.Equal(t, settlement.StateSecondChancePayment, f.repo.mustGet(t, s.AuctionID()).State(),
			"InvoicePaidV1 will finish the saga")
	})

	t.Run("decline at a settled saga is 409 offer-already-paid", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)
		f.repo.mutateLocked(t, s.AuctionID(), func(st *settlement.Settlement) error {
			return st.PaymentReceived(st.InvoiceID())
		})

		handleErr := f.handler.Handle(ctx, declineCmd(s))
		assert.ErrorIs(t, handleErr, errs.NewConflictError("offer-already-paid"))
	})

	t.Run("retry after the same outcome is the idempotent 204", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)
		require.NoError(t, f.handler.Handle(ctx, declineCmd(s)))
		voids, fails := len(f.billing.voids), len(f.auctions.markFails)

		require.NoError(t, f.handler.Handle(ctx, declineCmd(s)), "the HTTP retry is the same continuation")
		assert.Len(t, f.billing.voids, voids, "no repeated effects on the idempotent retry")
		assert.Len(t, f.auctions.markFails, fails)
	})

	t.Run("seam: crash after VoidInvoice before the commit", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)

		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handler.Handle(ctx, declineCmd(s)))
		require.Len(t, f.billing.voids, 1)
		assert.Equal(t, settlement.StateSecondChancePayment, f.repo.mustGet(t, s.AuctionID()).State())

		// The user's retry repeats the effects (void → already-voided →
		// nil on the billing side) and completes the fork.
		f.repo.updateErr = nil
		require.NoError(t, f.handler.Handle(ctx, declineCmd(s)))
		assert.Len(t, f.billing.voids, 2)
		assert.Equal(t, settlement.StateRelisted, f.repo.mustGet(t, s.AuctionID()).State())
	})

	t.Run("losing the race against expiry is benign: same outcome, 204", func(t *testing.T) {
		t.Parallel()
		f := newDeclineFixture(t)
		s := secondChanceSaga(t, f, 0)

		// The expiry worker commits its own relist between our effect
		// and commit phases (§6.4).
		f.repo.onUpdate = func() {
			f.repo.mutateLocked(t, s.AuctionID(), func(st *settlement.Settlement) error {
				return st.ApplyNextStep(settlement.StepRelist, settlement.ReasonPaymentTimeout)
			})
		}

		require.NoError(t, f.handler.Handle(ctx, declineCmd(s)), "the matching outcome is an idempotent success")
		final := f.repo.mustGet(t, s.AuctionID())
		assert.Equal(t, settlement.StateRelisted, final.State())
		assert.Equal(t, settlement.ReasonPaymentTimeout, final.FailureReason(), "the expiry won the benign race")
	})
}

func TestDeterministicIDs(t *testing.T) {
	t.Parallel()

	auctionID, err := settlement.NewAuctionID(uuid.New())
	require.NoError(t, err)
	otherID, err := settlement.NewAuctionID(uuid.New())
	require.NoError(t, err)

	assert.Equal(t, command.InvoiceIDForAttempt(auctionID, 1), command.InvoiceIDForAttempt(auctionID, 1),
		"the same auction and attempt always derive the same invoice id")
	assert.NotEqual(t, command.InvoiceIDForAttempt(auctionID, 1), command.InvoiceIDForAttempt(auctionID, 2))
	assert.NotEqual(t, command.InvoiceIDForAttempt(auctionID, 1), command.InvoiceIDForAttempt(otherID, 1))

	assert.Equal(t, command.RelistIDFor(auctionID), command.RelistIDFor(auctionID))
	assert.NotEqual(t, command.RelistIDFor(auctionID), auctionID, "the replacement listing has its own identity")
	assert.NotEqual(t, command.RelistIDFor(auctionID), command.RelistIDFor(otherID))
}
