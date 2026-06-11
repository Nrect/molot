package command

import (
	"context"
	"errors"

	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// IssueInvoice issues the bill to the auction winner (attempt 1) or the
// runner-up (attempt 2). Called only by the settlement saga through the
// billing facade. The InvoiceID is deterministic (uuidv5 of
// auctionID+":"+attempt, derived by the saga), so a redelivered command
// is idempotent: the duplicate hits ErrInvoiceAlreadyIssued — mapped to
// nil here — with UNIQUE(auction_id, attempt) as the second line.
type IssueInvoice struct {
	InvoiceID invoice.InvoiceID
	AuctionID invoice.AuctionID
	Debtor    invoice.BidderID
	Hammer    invoice.Money
	Attempt   invoice.Attempt
}

type IssueInvoiceHandler struct {
	repo   invoice.Repository
	policy invoice.CommissionPolicy
	term   invoice.PaymentTerm
	clock  clock
}

func NewIssueInvoiceHandler(
	repo invoice.Repository,
	policy invoice.CommissionPolicy,
	term invoice.PaymentTerm,
	clk clock,
) IssueInvoiceHandler {
	if repo == nil {
		panic("NewIssueInvoiceHandler: nil repo")
	}
	if term.IsZero() {
		panic("NewIssueInvoiceHandler: zero payment term")
	}
	if clk == nil {
		panic("NewIssueInvoiceHandler: nil clock")
	}
	return IssueInvoiceHandler{repo: repo, policy: policy, term: term, clock: clk}
}

func (h IssueInvoiceHandler) Handle(ctx context.Context, cmd IssueInvoice) error {
	inv, err := invoice.NewInvoice(
		cmd.InvoiceID, cmd.AuctionID, cmd.Debtor,
		cmd.Hammer, h.policy, h.term, cmd.Attempt, h.clock.Now(),
	)
	if err != nil {
		return errs.NewIncorrectInputError("invalid-invoice").WithCause(err)
	}

	err = h.repo.Add(ctx, inv)
	if errors.Is(err, invoice.ErrInvoiceAlreadyIssued) {
		return nil // idempotent redelivery: the invoice already exists
	}
	return err
}
