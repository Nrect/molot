// Package ports exposes the settlement context to the outside world:
// the strict-server HTTP handlers generated from
// api/openapi/settlement.yaml and the typed event subscriptions feeding
// the saga. Ports call ONLY app handlers — never adapters or the
// repository directly (rule 28).
package ports

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"molot/internal/common/auth"
	"molot/internal/common/errs"
	"molot/internal/common/server/httperr"
	"molot/internal/settlement/app"
	"molot/internal/settlement/app/command"
	"molot/internal/settlement/app/query"
	"molot/internal/settlement/domain/settlement"
)

// HTTPServer implements StrictServerInterface on top of the app catalog.
type HTTPServer struct {
	app app.Application
}

func NewHTTPServer(application app.Application) HTTPServer {
	if application.Commands.DeclineSecondChanceOffer == nil ||
		application.Queries.SettlementStatus == nil {
		panic("NewHTTPServer: incomplete application")
	}
	return HTTPServer{app: application}
}

// RegisterHTTP mounts the settlement endpoints on the authenticated
// /api router group. All errors funnel through the single
// httperr.RespondWithSlugError helper (rule 30).
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

// DeclineSecondChanceOffer lets the runner-up decline the offer without
// waiting for the payment term (§6.4). 204 also covers the idempotent
// retry whose outcome already matches.
func (h HTTPServer) DeclineSecondChanceOffer(
	ctx context.Context,
	request DeclineSecondChanceOfferRequestObject,
) (DeclineSecondChanceOfferResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	actor, err := settlement.NewBidderID(user.ID)
	if err != nil {
		return nil, errs.NewUnknownError("invalid-user-id").WithCause(err)
	}
	auctionID, err := settlement.NewAuctionID(request.AuctionID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-auction-id").WithCause(err)
	}

	if err := h.app.Commands.DeclineSecondChanceOffer.Handle(ctx, command.DeclineSecondChanceOffer{
		AuctionID: auctionID,
		Actor:     actor,
	}); err != nil {
		return nil, err
	}
	return DeclineSecondChanceOffer204Response{}, nil
}

// GetSettlementStatus is the operations-only saga view (§8).
func (h HTTPServer) GetSettlementStatus(
	ctx context.Context,
	request GetSettlementStatusRequestObject,
) (GetSettlementStatusResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if user.Role != auth.RoleOperations {
		return nil, errs.NewForbiddenError("operations-only")
	}
	auctionID, err := settlement.NewAuctionID(request.AuctionID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-auction-id").WithCause(err)
	}

	view, err := h.app.Queries.SettlementStatus.Handle(ctx, query.SettlementStatus{AuctionID: auctionID})
	if err != nil {
		return nil, err
	}
	return GetSettlementStatus200JSONResponse(toAPISettlement(view)), nil
}

func toAPISettlement(v query.SettlementView) Settlement {
	// The view comes from our own uuid columns; a parse failure is a
	// programming error surfaced by the Recoverer as 500.
	s := Settlement{
		AuctionId:         uuid.MustParse(v.AuctionID),
		State:             SettlementState(v.State),
		WinnerId:          uuid.MustParse(v.WinnerID),
		HammerMinor:       v.HammerMinor,
		Currency:          v.Currency,
		RunnerUpQualifies: v.RunnerUpQualifies,
		RelistGeneration:  v.RelistGeneration,
		Attempt:           v.Attempt,
	}
	if v.RunnerUpID != "" {
		runnerUp := uuid.MustParse(v.RunnerUpID)
		s.RunnerUpId = &runnerUp
		s.RunnerUpMinor = &v.RunnerUpMinor
	}
	if v.InvoiceID != "" {
		invoiceID := uuid.MustParse(v.InvoiceID)
		s.InvoiceId = &invoiceID
	}
	if v.FailureReason != "" {
		reason := SettlementFailureReason(v.FailureReason)
		s.FailureReason = &reason
	}
	return s
}
