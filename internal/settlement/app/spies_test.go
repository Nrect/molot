package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/app"
	"molot/internal/settlement/app/command"
	"molot/internal/settlement/domain/settlement"
)

// --- recording spies (rule 41) ----------------------------------------------

// sagaRepoSpy is an in-memory settlement.Repository with scripted
// failures and a pre-commit hook: enough to simulate every crash seam
// and commit race of §6.7 without Docker.
type sagaRepoSpy struct {
	mu        sync.Mutex
	byAuction map[settlement.AuctionID]settlement.Settlement

	updateErr   error  // scripted commit failure ("crash before commit")
	onUpdate    func() // runs under the lock before updateFn — a concurrent commit
	addCalls    int
	updateCalls int
}

func newSagaRepoSpy() *sagaRepoSpy {
	return &sagaRepoSpy{byAuction: map[settlement.AuctionID]settlement.Settlement{}}
}

func (r *sagaRepoSpy) Add(_ context.Context, s *settlement.Settlement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addCalls++
	if _, ok := r.byAuction[s.AuctionID()]; ok {
		return nil // ON CONFLICT (auction_id) DO NOTHING parity
	}
	r.byAuction[s.AuctionID()] = *s
	return nil
}

func (r *sagaRepoSpy) Get(_ context.Context, id settlement.AuctionID) (*settlement.Settlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.byAuction[id]
	if !ok {
		return nil, settlement.NotFoundError{AuctionID: id}
	}
	cp := stored
	return &cp, nil
}

func (r *sagaRepoSpy) Update(
	ctx context.Context,
	id settlement.AuctionID,
	updateFn func(ctx context.Context, s *settlement.Settlement) (*settlement.Settlement, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
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

// seed stores a saga directly (arrange-only shortcut for advanced states).
func (r *sagaRepoSpy) seed(s *settlement.Settlement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byAuction[s.AuctionID()] = *s
}

// mutateLocked applies a competing transition directly to the store —
// used from the onUpdate hook, which already holds the lock.
func (r *sagaRepoSpy) mutateLocked(t *testing.T, id settlement.AuctionID, fn func(s *settlement.Settlement) error) {
	t.Helper()
	stored, ok := r.byAuction[id]
	require.True(t, ok)
	require.NoError(t, fn(&stored))
	r.byAuction[id] = stored
}

func (r *sagaRepoSpy) mustGet(t *testing.T, id settlement.AuctionID) settlement.Settlement {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.byAuction[id]
	require.True(t, ok, "settlement %s must exist", id)
	return stored
}

// auctionGatewaySpy records the auction facade calls and plays
// scripted outcomes (idempotent "already done" = nil, like the facade).
type markFailCall struct {
	auctionID settlement.AuctionID
	reason    settlement.FailureReason
}

type relistCall struct {
	originalID, newID settlement.AuctionID
	startsAt, endsAt  time.Time
}

type auctionGatewaySpy struct {
	awards    []settlement.AuctionID
	confirms  []settlement.AuctionID
	markFails []markFailCall
	relists   []relistCall

	awardErr, confirmErr, markFailErr, relistErr error
	onMarkSaleFailed                             func() // race hook between effect and commit
}

func (g *auctionGatewaySpy) AwardToRunnerUp(_ context.Context, auctionID settlement.AuctionID) error {
	g.awards = append(g.awards, auctionID)
	return g.awardErr
}

func (g *auctionGatewaySpy) Relist(_ context.Context, originalID, newID settlement.AuctionID, startsAt, endsAt time.Time) error {
	g.relists = append(g.relists, relistCall{originalID: originalID, newID: newID, startsAt: startsAt, endsAt: endsAt})
	return g.relistErr
}

func (g *auctionGatewaySpy) MarkSaleFailed(_ context.Context, auctionID settlement.AuctionID, reason settlement.FailureReason) error {
	g.markFails = append(g.markFails, markFailCall{auctionID: auctionID, reason: reason})
	if g.onMarkSaleFailed != nil {
		hook := g.onMarkSaleFailed
		g.onMarkSaleFailed = nil
		hook()
	}
	return g.markFailErr
}

func (g *auctionGatewaySpy) ConfirmSettlement(_ context.Context, auctionID settlement.AuctionID) error {
	g.confirms = append(g.confirms, auctionID)
	return g.confirmErr
}

// billingGatewaySpy records IssueInvoice calls.
type issueCall struct {
	invoiceID settlement.InvoiceID
	auctionID settlement.AuctionID
	debtor    settlement.BidderID
	amount    settlement.Money
	attempt   int
}

type billingGatewaySpy struct {
	issues   []issueCall
	issueErr error
}

func (g *billingGatewaySpy) IssueInvoice(
	_ context.Context,
	invoiceID settlement.InvoiceID,
	auctionID settlement.AuctionID,
	debtor settlement.BidderID,
	amount settlement.Money,
	attempt int,
) error {
	g.issues = append(g.issues, issueCall{
		invoiceID: invoiceID, auctionID: auctionID, debtor: debtor, amount: amount, attempt: attempt,
	})
	return g.issueErr
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// --- fixture -----------------------------------------------------------------

var testNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

const (
	relistDelay    = time.Hour
	relistDuration = 24 * time.Hour
)

type fixture struct {
	repo     *sagaRepoSpy
	auctions *auctionGatewaySpy
	billing  *billingGatewaySpy
	handlers app.EventHandlers
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	relist, err := settlement.NewRelistPolicy(relistDelay, relistDuration)
	require.NoError(t, err)
	repo := newSagaRepoSpy()
	auctions := &auctionGatewaySpy{}
	billing := &billingGatewaySpy{}
	return &fixture{
		repo:     repo,
		auctions: auctions,
		billing:  billing,
		handlers: app.NewEventHandlers(repo, auctions, billing, relist, fixedClock{now: testNow}),
	}
}

type soldOpts struct {
	noRunnerUp bool
	qualifies  bool
	relistGen  int
}

// soldAuction builds an AuctionClosedV1-shaped payload for a sold lot.
func soldAuction(opts soldOpts) app.ClosedAuction {
	e := app.ClosedAuction{
		AuctionID:         uuid.New(),
		Outcome:           "sold",
		WinnerID:          uuid.New(),
		HammerMinor:       100_000,
		Currency:          "EUR",
		RunnerUpQualifies: opts.qualifies,
		RelistGeneration:  opts.relistGen,
	}
	if !opts.noRunnerUp {
		e.RunnerUpID = uuid.New()
		e.RunnerUpMinor = 90_000
	}
	return e
}

func auctionIDOf(t *testing.T, e app.ClosedAuction) settlement.AuctionID {
	t.Helper()
	id, err := settlement.NewAuctionID(e.AuctionID)
	require.NoError(t, err)
	return id
}

func firstInvoiceOf(t *testing.T, e app.ClosedAuction) settlement.InvoiceID {
	t.Helper()
	return command.InvoiceIDForAttempt(auctionIDOf(t, e), 1)
}

func secondInvoiceOf(t *testing.T, e app.ClosedAuction) settlement.InvoiceID {
	t.Helper()
	return command.InvoiceIDForAttempt(auctionIDOf(t, e), 2)
}

// closeSold drives the saga to AwaitingPayment through the real handler.
func (f *fixture) closeSold(t *testing.T, e app.ClosedAuction) {
	t.Helper()
	require.NoError(t, f.handlers.OnAuctionClosed(context.Background(), e))
	require.Equal(t, settlement.StateAwaitingPayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
}

// awardRunnerUp drives AwaitingPayment → SecondChancePayment through
// the real expiry and winner-reassigned handlers.
func (f *fixture) awardRunnerUp(t *testing.T, e app.ClosedAuction) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
	require.Equal(t, settlement.StateAwardingRunnerUp, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	require.NoError(t, f.handlers.OnWinnerReassigned(ctx, e.AuctionID, e.RunnerUpID, e.RunnerUpMinor, e.Currency))
	require.Equal(t, settlement.StateSecondChancePayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
}
