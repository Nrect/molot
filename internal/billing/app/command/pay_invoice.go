package command

import (
	"context"
	"errors"

	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// ChargeKey is the PSP idempotency key — by contract equal to the
// InvoiceID (§3.1): a repeated Charge with the same key never takes the
// money twice and returns the same reference.
type ChargeKey invoice.InvoiceID

func (k ChargeKey) String() string { return invoice.InvoiceID(k).String() }

// paymentGateway is the consumer-side PSP port (§3.1). Contract:
//   - Charge is idempotent per key: a retry returns the original
//     reference without a second debit. A network failure surfaces as
//     invoice.ErrPSPUnavailable, a refusal as invoice.ErrPaymentDeclined.
//   - Refund is idempotent and a no-op when no charge exists for the
//     key — safe to call as crash-seam insurance.
type paymentGateway interface {
	Charge(ctx context.Context, key ChargeKey, amount invoice.Money) (invoice.PaymentReference, error)
	Refund(ctx context.Context, key ChargeKey) error
}

// PayInvoice is the debtor paying their invoice. The handler implements
// the full PSP protocol with refund compensation (§3.1): Charge happens
// strictly BEFORE the database transaction, and a lost race against
// expiry/void is compensated with an idempotent Refund. Every crash
// seam recovers on the client's retry because both Charge and Refund
// are idempotent by key.
type PayInvoice struct {
	InvoiceID invoice.InvoiceID
	Payer     invoice.BidderID
}

type PayInvoiceHandler struct {
	repo  invoice.Repository
	psp   paymentGateway
	clock clock
}

func NewPayInvoiceHandler(repo invoice.Repository, psp paymentGateway, clk clock) PayInvoiceHandler {
	if repo == nil {
		panic("NewPayInvoiceHandler: nil repo")
	}
	if psp == nil {
		panic("NewPayInvoiceHandler: nil payment gateway")
	}
	if clk == nil {
		panic("NewPayInvoiceHandler: nil clock")
	}
	return PayInvoiceHandler{repo: repo, psp: psp, clock: clk}
}

func (h PayInvoiceHandler) Handle(ctx context.Context, cmd PayInvoice) error {
	key := ChargeKey(cmd.InvoiceID)

	// Step 1 — pre-check: a lock-free read by (id, payer). A missing or
	// foreign invoice is the same 404 (anti-enumeration).
	inv, err := h.repo.Get(ctx, cmd.InvoiceID, cmd.Payer)
	if err != nil {
		var notFound invoice.NotFoundError
		if errors.As(err, &notFound) {
			return errs.NewNotFoundError("invoice-not-found").WithCause(err)
		}
		return err
	}

	switch inv.Status() {
	case invoice.StatusPaid:
		// Retry after a successful payment: idempotent success.
		// Critically, Refund is NOT called — the money stays charged.
		return nil
	case invoice.StatusExpired, invoice.StatusVoided:
		// Insurance for the crash seam between a lost race and its
		// Refund: the retry lands here and repeats the idempotent
		// Refund (a no-op when nothing was charged).
		if err := h.psp.Refund(ctx, key); err != nil {
			return errs.NewUnavailableError("psp-unavailable").WithCause(err)
		}
		return errs.NewConflictError("invoice-no-longer-payable")
	case invoice.StatusPending:
		// Proceed to charge.
	default:
		panic("billing: unknown invoice status " + inv.Status().String())
	}

	// Step 2 — Charge, strictly before the transaction. The client
	// retries 502s; the repeated Charge is idempotent by key.
	ref, err := h.psp.Charge(ctx, key, inv.Total())
	switch {
	case errors.Is(err, invoice.ErrPaymentDeclined):
		return errs.NewConflictError("payment-declined").WithCause(err)
	case err != nil:
		return errs.NewUnavailableError("psp-unavailable").WithCause(err)
	}

	// Step 3 — MarkPaid under SELECT ... FOR UPDATE; the commit
	// publishes InvoicePaidV1 through the outbox.
	err = h.repo.Update(ctx, cmd.InvoiceID, cmd.Payer,
		func(_ context.Context, inv *invoice.Invoice) (*invoice.Invoice, error) {
			if err := inv.MarkPaid(ref, h.clock.Now()); err != nil {
				return nil, err
			}
			return inv, nil
		})

	// Step 4 — interpret the race outcome.
	switch {
	case err == nil:
		return nil
	case errors.Is(err, invoice.ErrInvoiceAlreadyPaid):
		// Concurrent duplicate of the same payment (same idempotency
		// key): success, and no Refund.
		return nil
	case errors.Is(err, invoice.ErrInvoiceExpired), errors.Is(err, invoice.ErrInvoiceVoided):
		// Race lost: the invoice expired or was voided between Charge
		// and the row lock — compensate the charge.
		if refundErr := h.psp.Refund(ctx, key); refundErr != nil {
			return errs.NewUnavailableError("psp-unavailable").WithCause(refundErr)
		}
		return errs.NewConflictError("invoice-no-longer-payable").WithCause(err)
	}
	var notFound invoice.NotFoundError
	if errors.As(err, &notFound) {
		return errs.NewNotFoundError("invoice-not-found").WithCause(err)
	}
	return err
}
