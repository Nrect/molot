// Package query holds the read side of billing: UI-shaped views read
// directly from the write tables (§5 — "not every query needs a read
// model"); the interface is declared next to its consumer.
package query

import (
	"context"
	"errors"
	"time"

	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// InvoiceView is the UI-shaped read result — not the domain aggregate,
// not an OpenAPI model, not a DB row (BOOK_AUDIT rule 26).
type InvoiceView struct {
	InvoiceID       string
	AuctionID       string
	Status          string
	HammerMinor     int64
	CommissionMinor int64
	TotalMinor      int64
	Currency        string
	DueAt           time.Time
	Attempt         int
}

// InvoiceReadModel reads invoices for their debtor. Ownership lives in
// the WHERE predicate (id AND debtor_id): a foreign invoice and a
// missing one are indistinguishable — both yield invoice.NotFoundError
// (anti-enumeration, §2.2).
type InvoiceReadModel interface {
	InvoiceByID(ctx context.Context, id invoice.InvoiceID, actor invoice.BidderID) (InvoiceView, error)
	PendingOfBidder(ctx context.Context, b invoice.BidderID) ([]InvoiceView, error)
}

// InvoiceByID returns one invoice to its debtor.
type InvoiceByID struct {
	InvoiceID invoice.InvoiceID
	Actor     invoice.BidderID
}

type InvoiceByIDHandler struct {
	readModel InvoiceReadModel
}

func NewInvoiceByIDHandler(readModel InvoiceReadModel) InvoiceByIDHandler {
	if readModel == nil {
		panic("NewInvoiceByIDHandler: nil read model")
	}
	return InvoiceByIDHandler{readModel: readModel}
}

func (h InvoiceByIDHandler) Handle(ctx context.Context, q InvoiceByID) (InvoiceView, error) {
	view, err := h.readModel.InvoiceByID(ctx, q.InvoiceID, q.Actor)
	if err != nil {
		var notFound invoice.NotFoundError
		if errors.As(err, &notFound) {
			return InvoiceView{}, errs.NewNotFoundError("invoice-not-found").WithCause(err)
		}
		return InvoiceView{}, err
	}
	return view, nil
}

// PendingInvoicesOfBidder lists the bidder's own pending invoices.
// The port guarantees Bidder == authenticated user before building the
// query (own path-ID — no enumeration surface, §8).
type PendingInvoicesOfBidder struct {
	Bidder invoice.BidderID
}

type PendingInvoicesOfBidderHandler struct {
	readModel InvoiceReadModel
}

func NewPendingInvoicesOfBidderHandler(readModel InvoiceReadModel) PendingInvoicesOfBidderHandler {
	if readModel == nil {
		panic("NewPendingInvoicesOfBidderHandler: nil read model")
	}
	return PendingInvoicesOfBidderHandler{readModel: readModel}
}

func (h PendingInvoicesOfBidderHandler) Handle(ctx context.Context, q PendingInvoicesOfBidder) ([]InvoiceView, error) {
	return h.readModel.PendingOfBidder(ctx, q.Bidder)
}
