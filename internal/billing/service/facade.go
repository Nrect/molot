package service

import (
	"context"

	"github.com/google/uuid"

	"molot/internal/billing/app"
	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// Facade is billing's synchronous command surface for the settlement
// saga (ARCHITECTURE.md §6: the saga defines a consumer-side
// billingGateway and adapts it to this facade). Signatures are
// deliberately primitive — uuid.UUID, int64, string, int — so the
// consumer needs no billing domain types.
//
// Both commands are idempotent ("already in the target state" → nil),
// which makes the saga's effect-phase retries safe (§6.1). Errors cross
// the context boundary only as errs.SlugError.
type Facade struct {
	commands app.Commands
}

func newFacade(application app.Application) Facade {
	return Facade{commands: application.Commands}
}

// IssueInvoice issues the bill for an auction to debtor: hammerMinor in
// minor units of currency, attempt 1 (winner) or 2 (second chance).
// The invoiceID is the saga's deterministic uuidv5 of
// auctionID+":"+attempt — a repeat for the same (auction, attempt) is a
// no-op (ErrInvoiceAlreadyIssued → nil), with UNIQUE(auction_id,
// attempt) as the second line.
func (f Facade) IssueInvoice(
	ctx context.Context,
	invoiceID, auctionID, debtor uuid.UUID,
	hammerMinor int64,
	currency string,
	attempt int,
) error {
	id, err := invoice.NewInvoiceID(invoiceID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-invoice-id").WithCause(err)
	}
	aucID, err := invoice.NewAuctionID(auctionID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-auction-id").WithCause(err)
	}
	debtorID, err := invoice.NewBidderID(debtor)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-debtor-id").WithCause(err)
	}
	cur, err := invoice.NewCurrency(currency)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-currency").WithCause(err)
	}
	hammer, err := invoice.NewMoney(hammerMinor, cur)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-hammer-price").WithCause(err)
	}
	att, err := invoice.NewAttempt(attempt)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-attempt").WithCause(err)
	}

	return f.commands.IssueInvoice.Handle(ctx, command.IssueInvoice{
		InvoiceID: id,
		AuctionID: aucID,
		Debtor:    debtorID,
		Hammer:    hammer,
		Attempt:   att,
	})
}

// VoidInvoice cancels a pending invoice (the runner-up declined the
// second-chance offer). Already voided/expired → nil (idempotent
// no-op). An already PAID invoice is NOT a no-op: the payment won and
// the call returns errs.NewConflictError("invoice-already-paid") — the
// saga maps it to 409 offer-already-paid and lets InvoicePaidV1 finish
// the settlement.
func (f Facade) VoidInvoice(ctx context.Context, invoiceID uuid.UUID) error {
	id, err := invoice.NewInvoiceID(invoiceID)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-invoice-id").WithCause(err)
	}
	return f.commands.VoidInvoice.Handle(ctx, command.VoidInvoice{InvoiceID: id})
}
