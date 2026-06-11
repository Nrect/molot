package command_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
)

// --- recording spies (BOOK_AUDIT rule 41) -----------------------------------

// repoSpy is a hand-written recording spy for invoice.Repository: one
// optional backing aggregate, recorded calls, scripted failures.
type repoSpy struct {
	inv *invoice.Invoice // backing aggregate; nil = empty repo

	addErr    error
	added     []*invoice.Invoice
	updateErr error // overrides the updateFn outcome when set

	updateCalls       int
	systemUpdateCalls int
}

func (s *repoSpy) Add(_ context.Context, inv *invoice.Invoice) error {
	s.added = append(s.added, inv)
	return s.addErr
}

func (s *repoSpy) Get(_ context.Context, id invoice.InvoiceID, actor invoice.BidderID) (*invoice.Invoice, error) {
	if s.inv == nil || s.inv.ID() != id || invoice.CanDebtorAccessInvoice(actor, *s.inv) != nil {
		return nil, invoice.NotFoundError{InvoiceID: id}
	}
	cp := *s.inv
	return &cp, nil
}

func (s *repoSpy) Update(
	ctx context.Context,
	id invoice.InvoiceID,
	actor invoice.BidderID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	s.updateCalls++
	if s.inv == nil || s.inv.ID() != id || invoice.CanDebtorAccessInvoice(actor, *s.inv) != nil {
		return invoice.NotFoundError{InvoiceID: id}
	}
	return s.runUpdate(ctx, updateFn)
}

func (s *repoSpy) UpdateAsSystem(
	ctx context.Context,
	id invoice.InvoiceID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	s.systemUpdateCalls++
	if s.inv == nil || s.inv.ID() != id {
		return invoice.NotFoundError{InvoiceID: id}
	}
	return s.runUpdate(ctx, updateFn)
}

func (s *repoSpy) runUpdate(
	ctx context.Context,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	work := *s.inv
	updated, err := updateFn(ctx, &work)
	if err != nil {
		return err
	}
	updated.PullDomainEvents()
	s.inv = updated
	return nil
}

func (s *repoSpy) PendingDueBefore(context.Context, time.Time, int) ([]invoice.InvoiceID, error) {
	return nil, nil
}

// gatewaySpy records Charge/Refund calls and plays scripted outcomes.
type gatewaySpy struct {
	ref       invoice.PaymentReference
	chargeErr error
	refundErr error

	charges []command.ChargeKey
	refunds []command.ChargeKey
}

func (g *gatewaySpy) Charge(_ context.Context, key command.ChargeKey, _ invoice.Money) (invoice.PaymentReference, error) {
	g.charges = append(g.charges, key)
	if g.chargeErr != nil {
		return invoice.PaymentReference{}, g.chargeErr
	}
	return g.ref, nil
}

func (g *gatewaySpy) Refund(_ context.Context, key command.ChargeKey) error {
	g.refunds = append(g.refunds, key)
	return g.refundErr
}

// fixedClock is the deterministic time source of the app tests.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// --- domain fixtures via the public API -------------------------------------

func testMoney(t *testing.T, amount int64) invoice.Money {
	t.Helper()
	cur, err := invoice.NewCurrency("USD")
	require.NoError(t, err)
	m, err := invoice.NewMoney(amount, cur)
	require.NoError(t, err)
	return m
}

func testPolicy(t *testing.T) invoice.CommissionPolicy {
	t.Helper()
	p, err := invoice.NewCommissionPolicy(1_000)
	require.NoError(t, err)
	return p
}

func testTerm(t *testing.T) invoice.PaymentTerm {
	t.Helper()
	pt, err := invoice.NewPaymentTerm(48 * time.Hour)
	require.NoError(t, err)
	return pt
}

func testRef(t *testing.T) invoice.PaymentReference {
	t.Helper()
	ref, err := invoice.NewPaymentReference("psp-" + uuid.NewString())
	require.NoError(t, err)
	return ref
}

// newInvoiceFixture issues a pending invoice; issuedInPast makes the
// due date already passed so Expire is legal.
func newInvoiceFixture(t *testing.T, issuedInPast bool) *invoice.Invoice {
	t.Helper()
	id, err := invoice.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	auctionID, err := invoice.NewAuctionID(uuid.New())
	require.NoError(t, err)
	debtor, err := invoice.NewBidderID(uuid.New())
	require.NoError(t, err)
	issuedAt := time.Now()
	if issuedInPast {
		issuedAt = issuedAt.Add(-72 * time.Hour)
	}
	inv, err := invoice.NewInvoice(
		id, auctionID, debtor, testMoney(t, 100_000),
		testPolicy(t), testTerm(t), invoice.AttemptFirst, issuedAt,
	)
	require.NoError(t, err)
	inv.PullDomainEvents()
	return inv
}
