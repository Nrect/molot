package invoice_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/domain/invoice"
)

// fixtures are built only through the domain API (BOOK_AUDIT rule 40).

func newTestIDs(t *testing.T) (invoice.InvoiceID, invoice.AuctionID, invoice.BidderID) {
	t.Helper()
	id, err := invoice.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	auctionID, err := invoice.NewAuctionID(uuid.New())
	require.NoError(t, err)
	debtor, err := invoice.NewBidderID(uuid.New())
	require.NoError(t, err)
	return id, auctionID, debtor
}

func money(t *testing.T, amount int64) invoice.Money {
	t.Helper()
	cur, err := invoice.NewCurrency("USD")
	require.NoError(t, err)
	m, err := invoice.NewMoney(amount, cur)
	require.NoError(t, err)
	return m
}

func policy(t *testing.T, bp int) invoice.CommissionPolicy {
	t.Helper()
	p, err := invoice.NewCommissionPolicy(bp)
	require.NoError(t, err)
	return p
}

func term(t *testing.T, d time.Duration) invoice.PaymentTerm {
	t.Helper()
	pt, err := invoice.NewPaymentTerm(d)
	require.NoError(t, err)
	return pt
}

func paymentRef(t *testing.T) invoice.PaymentReference {
	t.Helper()
	ref, err := invoice.NewPaymentReference("psp-" + uuid.NewString())
	require.NoError(t, err)
	return ref
}

// issueInvoice issues an invoice whose due date is already in the past
// (issued 72h ago with a 48h term), so Expire is legal "now".
func issueInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()
	id, auctionID, debtor := newTestIDs(t)
	inv, err := invoice.NewInvoice(
		id, auctionID, debtor, money(t, 100_000),
		policy(t, 1_000), term(t, 48*time.Hour), invoice.AttemptFirst,
		time.Now().Add(-72*time.Hour),
	)
	require.NoError(t, err)
	return inv
}

// invoiceInStatus drives a freshly issued invoice into status through
// the public behavior API only.
func invoiceInStatus(t *testing.T, status invoice.InvoiceStatus) *invoice.Invoice {
	t.Helper()
	inv := issueInvoice(t)
	inv.PullDomainEvents() // drop the issue event; tests assert transitions
	switch status {
	case invoice.StatusPending:
	case invoice.StatusPaid:
		require.NoError(t, inv.MarkPaid(paymentRef(t), time.Now()))
		inv.PullDomainEvents()
	case invoice.StatusExpired:
		require.NoError(t, inv.Expire(time.Now()))
		inv.PullDomainEvents()
	case invoice.StatusVoided:
		require.NoError(t, inv.Void(time.Now()))
		inv.PullDomainEvents()
	default:
		t.Fatalf("unknown status %s", status)
	}
	return inv
}

func TestNewInvoice(t *testing.T) {
	t.Parallel()

	id, auctionID, debtor := newTestIDs(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	inv, err := invoice.NewInvoice(
		id, auctionID, debtor, money(t, 100_000),
		policy(t, 1_000), term(t, 48*time.Hour), invoice.AttemptFirst, now,
	)
	require.NoError(t, err)

	assert.Equal(t, id, inv.ID())
	assert.Equal(t, auctionID, inv.AuctionID())
	assert.Equal(t, debtor, inv.Debtor())
	assert.Equal(t, int64(100_000), inv.Hammer().Amount())
	assert.Equal(t, int64(10_000), inv.Commission().Amount(), "10% of hammer")
	assert.Equal(t, int64(110_000), inv.Total().Amount(), "total = hammer + commission")
	assert.Equal(t, invoice.StatusPending, inv.Status())
	assert.True(t, inv.IsPending())
	assert.Equal(t, now.Add(48*time.Hour), inv.DueAt())
	assert.Equal(t, invoice.AttemptFirst, inv.Attempt())
	assert.True(t, inv.PSPRef().IsZero())
	assert.Equal(t, int64(1), inv.Version())

	events := inv.PullDomainEvents()
	require.Len(t, events, 1)
	issued, ok := events[0].(invoice.InvoiceIssued)
	require.True(t, ok, "expected InvoiceIssued, got %T", events[0])
	assert.Equal(t, id, issued.InvoiceID)
	assert.Equal(t, int64(110_000), issued.Total.Amount())
	assert.Equal(t, now, issued.OccurredAt)
	assert.Empty(t, inv.PullDomainEvents(), "events must be drained")
}

func TestNewInvoiceValidation(t *testing.T) {
	t.Parallel()

	id, auctionID, debtor := newTestIDs(t)
	hammer := money(t, 100_000)
	cp := policy(t, 1_000)
	pt := term(t, 48*time.Hour)
	now := time.Now()

	cases := []struct {
		name string
		run  func() error
	}{
		{"zero invoice id", func() error {
			_, err := invoice.NewInvoice(invoice.InvoiceID{}, auctionID, debtor, hammer, cp, pt, invoice.AttemptFirst, now)
			return err
		}},
		{"zero auction id", func() error {
			_, err := invoice.NewInvoice(id, invoice.AuctionID{}, debtor, hammer, cp, pt, invoice.AttemptFirst, now)
			return err
		}},
		{"zero debtor", func() error {
			_, err := invoice.NewInvoice(id, auctionID, invoice.BidderID{}, hammer, cp, pt, invoice.AttemptFirst, now)
			return err
		}},
		{"zero hammer", func() error {
			_, err := invoice.NewInvoice(id, auctionID, debtor, invoice.Money{}, cp, pt, invoice.AttemptFirst, now)
			return err
		}},
		{"zero payment term", func() error {
			_, err := invoice.NewInvoice(id, auctionID, debtor, hammer, cp, invoice.PaymentTerm{}, invoice.AttemptFirst, now)
			return err
		}},
		{"zero attempt", func() error {
			_, err := invoice.NewInvoice(id, auctionID, debtor, hammer, cp, pt, invoice.Attempt{}, now)
			return err
		}},
		{"zero time", func() error {
			_, err := invoice.NewInvoice(id, auctionID, debtor, hammer, cp, pt, invoice.AttemptFirst, time.Time{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Error(t, tc.run())
		})
	}
}

// TestInvoiceGuardTable covers the full transition matrix of §2.2 —
// every method against every status. "→ nil" no-op interpretation
// belongs to the command handlers, never the domain.
func TestInvoiceGuardTable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	markPaid := func(t *testing.T, inv *invoice.Invoice) error {
		return inv.MarkPaid(paymentRef(t), now)
	}
	expire := func(_ *testing.T, inv *invoice.Invoice) error { return inv.Expire(now) }
	void := func(_ *testing.T, inv *invoice.Invoice) error { return inv.Void(now) }

	cases := []struct {
		name       string
		from       invoice.InvoiceStatus
		op         func(*testing.T, *invoice.Invoice) error
		wantErr    error
		wantStatus invoice.InvoiceStatus // status after the call
	}{
		{"MarkPaid on Pending", invoice.StatusPending, markPaid, nil, invoice.StatusPaid},
		{"MarkPaid on Paid", invoice.StatusPaid, markPaid, invoice.ErrInvoiceAlreadyPaid, invoice.StatusPaid},
		{"MarkPaid on Expired", invoice.StatusExpired, markPaid, invoice.ErrInvoiceExpired, invoice.StatusExpired},
		{"MarkPaid on Voided", invoice.StatusVoided, markPaid, invoice.ErrInvoiceVoided, invoice.StatusVoided},

		{"Expire on Pending past due", invoice.StatusPending, expire, nil, invoice.StatusExpired},
		{"Expire on Paid", invoice.StatusPaid, expire, invoice.ErrInvoiceAlreadyPaid, invoice.StatusPaid},
		{"Expire on Expired", invoice.StatusExpired, expire, invoice.ErrInvoiceAlreadyExpired, invoice.StatusExpired},
		{"Expire on Voided", invoice.StatusVoided, expire, invoice.ErrInvoiceVoided, invoice.StatusVoided},

		{"Void on Pending", invoice.StatusPending, void, nil, invoice.StatusVoided},
		{"Void on Paid", invoice.StatusPaid, void, invoice.ErrInvoiceAlreadyPaid, invoice.StatusPaid},
		{"Void on Expired", invoice.StatusExpired, void, invoice.ErrInvoiceAlreadyExpired, invoice.StatusExpired},
		{"Void on Voided", invoice.StatusVoided, void, invoice.ErrInvoiceAlreadyVoided, invoice.StatusVoided},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := invoiceInStatus(t, tc.from)
			err := tc.op(t, inv)

			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Len(t, inv.PullDomainEvents(), 1, "exactly the transition's event must be recorded")
			} else {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, inv.PullDomainEvents(), "a refused transition must record nothing")
			}
			assert.Equal(t, tc.wantStatus, inv.Status())
		})
	}
}

func TestExpireBeforeDueIsRefused(t *testing.T) {
	t.Parallel()

	id, auctionID, debtor := newTestIDs(t)
	now := time.Now()
	inv, err := invoice.NewInvoice(
		id, auctionID, debtor, money(t, 50_000),
		policy(t, 500), term(t, 48*time.Hour), invoice.AttemptFirst, now,
	)
	require.NoError(t, err)
	inv.PullDomainEvents()

	require.ErrorIs(t, inv.Expire(now.Add(time.Hour)), invoice.ErrInvoiceNotDue)
	assert.Equal(t, invoice.StatusPending, inv.Status())
	assert.Empty(t, inv.PullDomainEvents())

	// Exactly at the deadline expiry is legal.
	require.NoError(t, inv.Expire(inv.DueAt()))
	assert.Equal(t, invoice.StatusExpired, inv.Status())
}

func TestMarkPaidSetsReferenceAndRecordsEvent(t *testing.T) {
	t.Parallel()

	inv := invoiceInStatus(t, invoice.StatusPending)
	ref := paymentRef(t)
	now := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)

	require.NoError(t, inv.MarkPaid(ref, now))

	assert.Equal(t, invoice.StatusPaid, inv.Status())
	assert.Equal(t, ref, inv.PSPRef())
	events := inv.PullDomainEvents()
	require.Len(t, events, 1)
	paid, ok := events[0].(invoice.InvoicePaid)
	require.True(t, ok, "expected InvoicePaid, got %T", events[0])
	assert.Equal(t, inv.ID(), paid.InvoiceID)
	assert.Equal(t, ref, paid.Reference)
	assert.Equal(t, now, paid.OccurredAt)
}

func TestMarkPaidRequiresReference(t *testing.T) {
	t.Parallel()

	inv := invoiceInStatus(t, invoice.StatusPending)
	require.ErrorIs(t, inv.MarkPaid(invoice.PaymentReference{}, time.Now()), invoice.ErrEmptyPaymentReference)
	assert.Equal(t, invoice.StatusPending, inv.Status())
}

func TestCanDebtorAccessInvoice(t *testing.T) {
	t.Parallel()

	inv := issueInvoice(t)

	assert.NoError(t, invoice.CanDebtorAccessInvoice(inv.Debtor(), *inv))

	stranger, err := invoice.NewBidderID(uuid.New())
	require.NoError(t, err)
	accessErr := invoice.CanDebtorAccessInvoice(stranger, *inv)
	var forbidden invoice.ForbiddenInvoiceAccessError
	require.ErrorAs(t, accessErr, &forbidden)
	assert.Equal(t, stranger, forbidden.Actor)
	assert.Equal(t, inv.Debtor(), forbidden.Debtor)
}

func TestUnmarshalFromDatabaseRecordsNoEvents(t *testing.T) {
	t.Parallel()

	id, auctionID, debtor := newTestIDs(t)
	inv, err := invoice.UnmarshalFromDatabase(
		id, auctionID, debtor,
		money(t, 100_000), money(t, 10_000), money(t, 110_000),
		invoice.StatusPaid, time.Now().Add(24*time.Hour), invoice.AttemptSecondChance,
		paymentRef(t), 3,
	)
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusPaid, inv.Status())
	assert.Equal(t, int64(3), inv.Version())
	assert.Empty(t, inv.PullDomainEvents())
}
