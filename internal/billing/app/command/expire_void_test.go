package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

func TestExpireInvoice(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("expires a due pending invoice as system", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, true)
		repo := &repoSpy{inv: inv}
		h := command.NewExpireInvoiceHandler(repo, fixedClock{now: now})

		require.NoError(t, h.Handle(context.Background(), command.ExpireInvoice{InvoiceID: inv.ID()}))

		assert.Equal(t, invoice.StatusExpired, repo.inv.Status())
		assert.Equal(t, 1, repo.systemUpdateCalls, "worker flow must use UpdateAsSystem")
		assert.Zero(t, repo.updateCalls)
	})

	t.Run("already terminal statuses are no-ops", func(t *testing.T) {
		t.Parallel()
		terminal := map[string]func(*invoice.Invoice) error{
			"paid":    func(i *invoice.Invoice) error { return i.MarkPaid(testRef(t), now) },
			"expired": func(i *invoice.Invoice) error { return i.Expire(now) },
			"voided":  func(i *invoice.Invoice) error { return i.Void(now) },
		}
		for name, transition := range terminal {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				inv := newInvoiceFixture(t, true)
				require.NoError(t, transition(inv))
				inv.PullDomainEvents()
				repo := &repoSpy{inv: inv}
				h := command.NewExpireInvoiceHandler(repo, fixedClock{now: now})

				assert.NoError(t, h.Handle(context.Background(), command.ExpireInvoice{InvoiceID: inv.ID()}))
			})
		}
	})

	t.Run("not yet due is a conflict", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false) // due in the future
		repo := &repoSpy{inv: inv}
		h := command.NewExpireInvoiceHandler(repo, fixedClock{now: now})

		err := h.Handle(context.Background(), command.ExpireInvoice{InvoiceID: inv.ID()})
		assert.True(t, errors.Is(err, errs.NewConflictError("invoice-not-due")))
		assert.True(t, repo.inv.IsPending())
	})

	t.Run("missing invoice is 404", func(t *testing.T) {
		t.Parallel()
		repo := &repoSpy{}
		h := command.NewExpireInvoiceHandler(repo, fixedClock{now: now})
		id, err := invoice.NewInvoiceID(uuid.New())
		require.NoError(t, err)

		handleErr := h.Handle(context.Background(), command.ExpireInvoice{InvoiceID: id})
		assert.Equal(t, errs.ErrorKindNotFound, errs.KindFromError(handleErr))
	})
}

func TestVoidInvoice(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("voids a pending invoice", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		repo := &repoSpy{inv: inv}
		h := command.NewVoidInvoiceHandler(repo, fixedClock{now: now})

		require.NoError(t, h.Handle(context.Background(), command.VoidInvoice{InvoiceID: inv.ID()}))
		assert.Equal(t, invoice.StatusVoided, repo.inv.Status())
		assert.Equal(t, 1, repo.systemUpdateCalls)
	})

	t.Run("already voided and already expired are no-ops", func(t *testing.T) {
		t.Parallel()
		for name, transition := range map[string]func(*invoice.Invoice) error{
			"voided":  func(i *invoice.Invoice) error { return i.Void(now) },
			"expired": func(i *invoice.Invoice) error { return i.Expire(now) },
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				inv := newInvoiceFixture(t, true)
				require.NoError(t, transition(inv))
				inv.PullDomainEvents()
				repo := &repoSpy{inv: inv}
				h := command.NewVoidInvoiceHandler(repo, fixedClock{now: now})

				assert.NoError(t, h.Handle(context.Background(), command.VoidInvoice{InvoiceID: inv.ID()}))
			})
		}
	})

	t.Run("already paid is propagated — payment won", func(t *testing.T) {
		t.Parallel()
		inv := newInvoiceFixture(t, false)
		require.NoError(t, inv.MarkPaid(testRef(t), now))
		inv.PullDomainEvents()
		repo := &repoSpy{inv: inv}
		h := command.NewVoidInvoiceHandler(repo, fixedClock{now: now})

		err := h.Handle(context.Background(), command.VoidInvoice{InvoiceID: inv.ID()})
		assert.True(t, errors.Is(err, errs.NewConflictError("invoice-already-paid")))
		assert.ErrorIs(t, err, invoice.ErrInvoiceAlreadyPaid, "the domain sentinel stays visible through the slug")
	})
}
