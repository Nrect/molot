// Package ports exposes the billing context to the outside world: the
// strict-server HTTP handlers generated from api/openapi/billing.yaml
// and the expiry worker. Ports call ONLY app.Application handlers —
// never adapters or the repository directly (BOOK_AUDIT rule 28).
package ports

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"molot/internal/billing/app"
	"molot/internal/billing/app/command"
	"molot/internal/billing/app/query"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/auth"
	"molot/internal/common/errs"
	"molot/internal/common/server/httperr"
)

// HTTPServer implements StrictServerInterface on top of the app catalog.
type HTTPServer struct {
	app app.Application
}

func NewHTTPServer(application app.Application) HTTPServer {
	if application.Commands.PayInvoice == nil ||
		application.Queries.InvoiceByID == nil ||
		application.Queries.PendingInvoicesOfBidder == nil {
		panic("NewHTTPServer: incomplete application")
	}
	return HTTPServer{app: application}
}

// RegisterHTTP mounts the billing endpoints on the authenticated /api
// router group. All errors funnel through the single
// httperr.RespondWithSlugError helper (BOOK_AUDIT rule 30).
func RegisterHTTP(api chi.Router, application app.Application) {
	strict := NewStrictHandlerWithOptions(NewHTTPServer(application), nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httperr.RespondWithSlugError(errs.NewIncorrectInputError("invalid-request").WithCause(err), w, r)
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httperr.RespondWithSlugError(err, w, r)
		},
	})
	HandlerFromMux(strict, api)
}

// GetInvoice returns the invoice to its debtor. Ownership is enforced
// in the WHERE predicate of the read model: a foreign invoice answers
// the same 404 invoice-not-found as a missing one (anti-enumeration).
func (h HTTPServer) GetInvoice(ctx context.Context, request GetInvoiceRequestObject) (GetInvoiceResponseObject, error) {
	actor, err := actorFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	id, err := invoice.NewInvoiceID(request.InvoiceID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-invoice-id").WithCause(err)
	}

	view, err := h.app.Queries.InvoiceByID.Handle(ctx, query.InvoiceByID{InvoiceID: id, Actor: actor})
	if err != nil {
		return nil, err
	}
	return GetInvoice200JSONResponse(toAPIInvoice(view)), nil
}

// GetBidderPendingInvoices lists the caller's own pending invoices.
// The path bidder must be the authenticated user — the path carries the
// caller's own ID, so the 403 leaks nothing (§8).
func (h HTTPServer) GetBidderPendingInvoices(ctx context.Context, request GetBidderPendingInvoicesRequestObject) (GetBidderPendingInvoicesResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if user.ID != request.BidderID {
		return nil, errs.NewForbiddenError("not-own-invoices")
	}
	if request.Params.Status != nil && *request.Params.Status != GetBidderPendingInvoicesParamsStatusPending {
		return nil, errs.NewIncorrectInputError("unsupported-status-filter")
	}
	bidder, err := invoice.NewBidderID(request.BidderID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-bidder-id").WithCause(err)
	}

	views, err := h.app.Queries.PendingInvoicesOfBidder.Handle(ctx, query.PendingInvoicesOfBidder{Bidder: bidder})
	if err != nil {
		return nil, err
	}
	list := InvoiceList{Invoices: make([]Invoice, 0, len(views))}
	for _, v := range views {
		list.Invoices = append(list.Invoices, toAPIInvoice(v))
	}
	return GetBidderPendingInvoices200JSONResponse(list), nil
}

// PayInvoice runs the PSP payment protocol (§3.1). 204 covers both the
// first successful payment and the idempotent retry of a paid invoice.
func (h HTTPServer) PayInvoice(ctx context.Context, request PayInvoiceRequestObject) (PayInvoiceResponseObject, error) {
	actor, err := actorFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	id, err := invoice.NewInvoiceID(request.InvoiceID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-invoice-id").WithCause(err)
	}

	if err := h.app.Commands.PayInvoice.Handle(ctx, command.PayInvoice{InvoiceID: id, Payer: actor}); err != nil {
		return nil, err
	}
	return PayInvoice204Response{}, nil
}

func actorFromCtx(ctx context.Context) (invoice.BidderID, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return invoice.BidderID{}, err
	}
	actor, err := invoice.NewBidderID(user.ID)
	if err != nil {
		return invoice.BidderID{}, errs.NewUnknownError("invalid-user-id").WithCause(err)
	}
	return actor, nil
}

func toAPIInvoice(v query.InvoiceView) Invoice {
	// The view comes from our own uuid columns; a parse failure is a
	// programming error surfaced by the Recoverer as 500.
	return Invoice{
		InvoiceId:       uuid.MustParse(v.InvoiceID),
		AuctionId:       uuid.MustParse(v.AuctionID),
		Status:          InvoiceStatus(v.Status),
		HammerMinor:     v.HammerMinor,
		CommissionMinor: v.CommissionMinor,
		TotalMinor:      v.TotalMinor,
		Currency:        v.Currency,
		DueAt:           v.DueAt,
		Attempt:         v.Attempt,
	}
}
