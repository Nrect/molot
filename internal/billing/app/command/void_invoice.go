package command

import (
	"context"
	"errors"

	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// VoidInvoice cancels the second-chance invoice when the runner-up
// declines the offer. Called only by the settlement saga through the
// facade (system flow). Already-voided/already-expired are idempotent
// no-ops; ErrInvoiceAlreadyPaid is deliberately propagated — the
// payment won and the saga must surface 409 offer-already-paid (§2.2).
type VoidInvoice struct {
	InvoiceID invoice.InvoiceID
}

type VoidInvoiceHandler struct {
	repo  invoice.Repository
	clock clock
}

func NewVoidInvoiceHandler(repo invoice.Repository, clk clock) VoidInvoiceHandler {
	if repo == nil {
		panic("NewVoidInvoiceHandler: nil repo")
	}
	if clk == nil {
		panic("NewVoidInvoiceHandler: nil clock")
	}
	return VoidInvoiceHandler{repo: repo, clock: clk}
}

func (h VoidInvoiceHandler) Handle(ctx context.Context, cmd VoidInvoice) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.InvoiceID,
		func(_ context.Context, inv *invoice.Invoice) (*invoice.Invoice, error) {
			if err := inv.Void(h.clock.Now()); err != nil {
				return nil, err
			}
			return inv, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, invoice.ErrInvoiceAlreadyVoided),
		errors.Is(err, invoice.ErrInvoiceAlreadyExpired):
		// Already in (or past) the target state: idempotent no-op for
		// the saga's retry.
		return nil
	case errors.Is(err, invoice.ErrInvoiceAlreadyPaid):
		// NOT a no-op: payment won the race; the saga turns this into
		// 409 offer-already-paid and lets InvoicePaidV1 finish the flow.
		return errs.NewConflictError("invoice-already-paid").WithCause(err)
	}
	var notFound invoice.NotFoundError
	if errors.As(err, &notFound) {
		return errs.NewNotFoundError("invoice-not-found").WithCause(err)
	}
	return err
}
