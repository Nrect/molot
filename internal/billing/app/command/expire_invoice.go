package command

import (
	"context"
	"errors"

	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// ExpireInvoice is the expiry worker's command (system flow —
// UpdateAsSystem, no acting user). Idempotency lives in the aggregate's
// guard table, not in the worker: already-terminal sentinels map to nil.
type ExpireInvoice struct {
	InvoiceID invoice.InvoiceID
}

type ExpireInvoiceHandler struct {
	repo  invoice.Repository
	clock clock
}

func NewExpireInvoiceHandler(repo invoice.Repository, clk clock) ExpireInvoiceHandler {
	if repo == nil {
		panic("NewExpireInvoiceHandler: nil repo")
	}
	if clk == nil {
		panic("NewExpireInvoiceHandler: nil clock")
	}
	return ExpireInvoiceHandler{repo: repo, clock: clk}
}

func (h ExpireInvoiceHandler) Handle(ctx context.Context, cmd ExpireInvoice) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.InvoiceID,
		func(_ context.Context, inv *invoice.Invoice) (*invoice.Invoice, error) {
			if err := inv.Expire(h.clock.Now()); err != nil {
				return nil, err
			}
			return inv, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, invoice.ErrInvoiceAlreadyPaid),
		errors.Is(err, invoice.ErrInvoiceAlreadyExpired),
		errors.Is(err, invoice.ErrInvoiceVoided):
		// Already terminal — the worker's repeated/concurrent pass is a
		// no-op; the losing path publishes nothing (§6.5).
		return nil
	case errors.Is(err, invoice.ErrInvoiceNotDue):
		return errs.NewConflictError("invoice-not-due").WithCause(err)
	}
	var notFound invoice.NotFoundError
	if errors.As(err, &notFound) {
		return errs.NewNotFoundError("invoice-not-found").WithCause(err)
	}
	return err
}
