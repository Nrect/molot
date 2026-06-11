package command_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// TestPayInvoiceProtocol covers every branch of the PSP protocol §3.1
// against scripted fake-PSP outcomes.
func TestPayInvoiceProtocol(t *testing.T) {
	t.Parallel()

	now := time.Now()
	newHandler := func(repo *repoSpy, psp *gatewaySpy) command.PayInvoiceHandler {
		return command.NewPayInvoiceHandler(repo, psp, fixedClock{now: now})
	}
	payCmd := func(inv *invoice.Invoice) command.PayInvoice {
		return command.PayInvoice{InvoiceID: inv.ID(), Payer: inv.Debtor()}
	}

	t.Run("unknown invoice is 404, psp never touched", func(t *testing.T) {
		t.Parallel()
		repo, psp := &repoSpy{}, &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)
		id, err := invoice.NewInvoiceID(uuid.New())
		require.NoError(t, err)
		payer, err := invoice.NewBidderID(uuid.New())
		require.NoError(t, err)

		handleErr := h.Handle(context.Background(), command.PayInvoice{InvoiceID: id, Payer: payer})

		assert.Equal(t, errs.ErrorKindNotFound, errs.KindFromError(handleErr))
		assert.Empty(t, psp.charges)
		assert.Empty(t, psp.refunds)
	})

	t.Run("foreign invoice is the same 404 (anti-enumeration)", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		repo, psp := &repoSpy{inv: inv}, &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)
		stranger, err := invoice.NewBidderID(uuid.New())
		require.NoError(t, err)

		handleErr := h.Handle(context.Background(), command.PayInvoice{InvoiceID: inv.ID(), Payer: stranger})

		assert.Equal(t, errs.ErrorKindNotFound, errs.KindFromError(handleErr))
		assert.True(t, errors.Is(handleErr, errs.NewNotFoundError("invoice-not-found")))
		assert.Empty(t, psp.charges)
	})

	t.Run("retry of a paid invoice is 204 and never refunds", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		require.NoError(t, inv.MarkPaid(testRef(t), now))
		inv.PullDomainEvents()
		repo, psp := &repoSpy{inv: inv}, &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)

		require.NoError(t, h.Handle(context.Background(), payCmd(inv)))

		assert.Empty(t, psp.charges, "no second charge")
		assert.Empty(t, psp.refunds, "refund after a successful payment would lose the platform's money")
	})

	t.Run("expired invoice: insurance refund then conflict", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, true)
		require.NoError(t, inv.Expire(now))
		inv.PullDomainEvents()
		repo, psp := &repoSpy{inv: inv}, &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewConflictError("invoice-no-longer-payable")))
		assert.Empty(t, psp.charges)
		require.Len(t, psp.refunds, 1, "crash-seam insurance: refund repeats on the retry path")
		assert.Equal(t, command.ChargeKey(inv.ID()), psp.refunds[0])
	})

	t.Run("voided invoice: insurance refund then conflict", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		require.NoError(t, inv.Void(now))
		inv.PullDomainEvents()
		repo, psp := &repoSpy{inv: inv}, &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewConflictError("invoice-no-longer-payable")))
		assert.Len(t, psp.refunds, 1)
	})

	t.Run("insurance refund failure is 502", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, true)
		require.NoError(t, inv.Expire(now))
		inv.PullDomainEvents()
		repo := &repoSpy{inv: inv}
		psp := &gatewaySpy{ref: testRef(t), refundErr: fmt.Errorf("net: %w", invoice.ErrPSPUnavailable)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.Equal(t, errs.ErrorKindUnavailable, errs.KindFromError(handleErr))
	})

	t.Run("psp network failure is retryable 502, invoice untouched", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		repo := &repoSpy{inv: inv}
		psp := &gatewaySpy{chargeErr: fmt.Errorf("net: %w", invoice.ErrPSPUnavailable)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewUnavailableError("psp-unavailable")))
		assert.True(t, repo.inv.IsPending(), "MarkPaid must not run after a failed charge")
		assert.Zero(t, repo.updateCalls)
		assert.Empty(t, psp.refunds)
	})

	t.Run("declined charge is 409, no refund", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		repo := &repoSpy{inv: inv}
		psp := &gatewaySpy{chargeErr: fmt.Errorf("refused: %w", invoice.ErrPaymentDeclined)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewConflictError("payment-declined")))
		assert.True(t, repo.inv.IsPending())
		assert.Empty(t, psp.refunds, "nothing was charged — nothing to refund")
	})

	t.Run("happy path: charge then mark paid", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		ref := testRef(t)
		repo, psp := &repoSpy{inv: inv}, &gatewaySpy{ref: ref}
		h := newHandler(repo, psp)

		require.NoError(t, h.Handle(context.Background(), payCmd(inv)))

		require.Len(t, psp.charges, 1)
		assert.Equal(t, command.ChargeKey(inv.ID()), psp.charges[0])
		assert.Equal(t, invoice.StatusPaid, repo.inv.Status())
		assert.Equal(t, ref, repo.inv.PSPRef())
		assert.Empty(t, psp.refunds)
	})

	t.Run("race lost to expiry: compensating refund then conflict", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, true)
		repo := &repoSpy{inv: inv, updateErr: invoice.ErrInvoiceExpired}
		psp := &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewConflictError("invoice-no-longer-payable")))
		assert.Len(t, psp.charges, 1, "charge happened before the lock")
		require.Len(t, psp.refunds, 1, "the lost race must be compensated")
		assert.Equal(t, command.ChargeKey(inv.ID()), psp.refunds[0])
	})

	t.Run("race lost to void: compensating refund then conflict", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		repo := &repoSpy{inv: inv, updateErr: invoice.ErrInvoiceVoided}
		psp := &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewConflictError("invoice-no-longer-payable")))
		assert.Len(t, psp.refunds, 1)
	})

	t.Run("refund failure after a lost race is 502 (retry repeats the refund)", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, true)
		repo := &repoSpy{inv: inv, updateErr: invoice.ErrInvoiceExpired}
		psp := &gatewaySpy{ref: testRef(t), refundErr: fmt.Errorf("net: %w", invoice.ErrPSPUnavailable)}
		h := newHandler(repo, psp)

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.Equal(t, errs.ErrorKindUnavailable, errs.KindFromError(handleErr))
	})

	t.Run("concurrent duplicate of the same payment is 204, no refund", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		repo := &repoSpy{inv: inv, updateErr: invoice.ErrInvoiceAlreadyPaid}
		psp := &gatewaySpy{ref: testRef(t)}
		h := newHandler(repo, psp)

		require.NoError(t, h.Handle(context.Background(), payCmd(inv)))

		assert.Empty(t, psp.refunds, "same idempotency key — the money is charged exactly once")
	})
}
