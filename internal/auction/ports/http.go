// Package ports exposes the auction context to the outside world: the
// strict-server HTTP handlers (openapi.gen.go — generated, never
// hand-edited), the event subscriptions and the closing worker. Ports
// call only app handlers (rule 28) and translate slug errors with the
// single common helper (rule 30).
package ports

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"molot/internal/auction/app"
	"molot/internal/auction/app/command"
	"molot/internal/auction/app/query"
	"molot/internal/auction/domain/auction"
	"molot/internal/common/auth"
	"molot/internal/common/errs"
)

// HTTPServer implements the generated StrictServerInterface on top of
// app.Application. The acting user always comes from the JWT context
// (typed auth.User) and is handed to commands as a domain type (§8).
type HTTPServer struct {
	app app.Application
}

func NewHTTPServer(application app.Application) HTTPServer {
	if application.Commands.ListAuction == nil || application.Queries.AuctionCard == nil {
		panic("NewHTTPServer: application is not fully assembled")
	}
	return HTTPServer{app: application}
}

func (s HTTPServer) ListAuction(ctx context.Context, request ListAuctionRequestObject) (ListAuctionResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	seller, err := auction.NewSellerID(user.ID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	auctionID, err := auction.NewAuctionID(request.Body.Id)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	currency, err := auction.NewCurrency(request.Body.Currency)
	if err != nil {
		return nil, errs.NewIncorrectInputError("unsupported-currency").WithCause(err)
	}
	startPrice, err := auction.NewMoney(request.Body.StartPriceMinor, currency)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-listing").WithCause(err)
	}
	increment, err := auction.NewMoney(request.Body.IncrementMinor, currency)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-increment").WithCause(err)
	}
	reserve := auction.NoReserve()
	if request.Body.ReservePriceMinor != nil {
		reserveMoney, err := auction.NewMoney(*request.Body.ReservePriceMinor, currency)
		if err != nil {
			return nil, errs.NewIncorrectInputError("invalid-reserve").WithCause(err)
		}
		if reserve, err = auction.NewReservePrice(reserveMoney); err != nil {
			return nil, errs.NewIncorrectInputError("invalid-reserve").WithCause(err)
		}
	}
	description := ""
	if request.Body.Description != nil {
		description = *request.Body.Description
	}
	lot, err := auction.NewLot(request.Body.Title, description)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-lot").WithCause(err)
	}
	window, err := auction.NewBiddingWindow(request.Body.StartsAt, request.Body.EndsAt)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-window").WithCause(err)
	}

	if err := s.app.Commands.ListAuction.Handle(ctx, command.ListAuction{
		AuctionID:  auctionID,
		Seller:     seller,
		Lot:        lot,
		StartPrice: startPrice,
		Increment:  increment,
		Reserve:    reserve,
		Window:     window,
	}); err != nil {
		return nil, err
	}

	location := "/api/auctions/" + auctionID.String()
	return ListAuction204Response{
		Headers: ListAuction204ResponseHeaders{ContentLocation: &location},
	}, nil
}

func (s HTTPServer) ActiveAuctionsCatalog(ctx context.Context, request ActiveAuctionsCatalogRequestObject) (ActiveAuctionsCatalogResponseObject, error) {
	q := query.ActiveAuctionsCatalog{}
	if request.Params.Page != nil {
		q.Page = *request.Params.Page
	}
	if request.Params.PageSize != nil {
		q.PageSize = *request.Params.PageSize
	}
	page, err := s.app.Queries.ActiveAuctionsCatalog.Handle(ctx, q)
	if err != nil {
		return nil, err
	}

	items := make([]CatalogItem, 0, len(page.Items))
	for _, item := range page.Items {
		auctionID, err := uuid.Parse(item.AuctionID)
		if err != nil {
			return nil, fmt.Errorf("invalid auction id in catalog: %w", err)
		}
		sellerID, err := uuid.Parse(item.SellerID)
		if err != nil {
			return nil, fmt.Errorf("invalid seller id in catalog: %w", err)
		}
		items = append(items, CatalogItem{
			AuctionId:           auctionID,
			Title:               item.Title,
			SellerId:            sellerID,
			CurrentPriceMinor:   item.CurrentPriceMinor,
			MinimalNextBidMinor: item.MinimalNextBidMinor,
			Currency:            item.Currency,
			BidCount:            item.BidCount,
			EndsAt:              item.EndsAt,
			EndingSoon:          item.EndingSoon,
		})
	}
	return ActiveAuctionsCatalog200JSONResponse(CatalogPage{
		Items: items, Total: page.Total, Page: page.Page,
	}), nil
}

func (s HTTPServer) AuctionCard(ctx context.Context, request AuctionCardRequestObject) (AuctionCardResponseObject, error) {
	auctionID, err := auction.NewAuctionID(request.AuctionID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	view, err := s.app.Queries.AuctionCard.Handle(ctx, query.AuctionCard{AuctionID: auctionID})
	if err != nil {
		return nil, err
	}

	card := AuctionCard{
		AuctionId:           request.AuctionID,
		Title:               view.Title,
		Description:         view.Description,
		Status:              view.Status,
		StartPriceMinor:     view.StartPriceMinor,
		CurrentPriceMinor:   view.CurrentPriceMinor,
		MinimalNextBidMinor: view.MinimalNextBidMinor,
		IncrementMinor:      view.IncrementMinor,
		Currency:            view.Currency,
		HasReserve:          view.HasReserve,
		StartsAt:            view.StartsAt,
		EndsAt:              view.EndsAt,
		ExtensionsUsed:      view.ExtensionsUsed,
		BidCount:            view.BidCount,
		RecentBids:          toBidViews(view.RecentBids),
	}
	if card.SellerId, err = uuid.Parse(view.SellerID); err != nil {
		return nil, fmt.Errorf("invalid seller id in card: %w", err)
	}
	if view.Outcome != "" {
		card.Outcome = &view.Outcome
	}
	if view.LeaderID != "" {
		leader, err := uuid.Parse(view.LeaderID)
		if err != nil {
			return nil, fmt.Errorf("invalid leader id in card: %w", err)
		}
		card.LeaderId = &leader
	}
	return AuctionCard200JSONResponse(card), nil
}

func (s HTTPServer) BidHistory(ctx context.Context, request BidHistoryRequestObject) (BidHistoryResponseObject, error) {
	auctionID, err := auction.NewAuctionID(request.AuctionID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	q := query.BidHistory{AuctionID: auctionID}
	if request.Params.Page != nil {
		q.Page = *request.Params.Page
	}
	if request.Params.PageSize != nil {
		q.PageSize = *request.Params.PageSize
	}
	page, err := s.app.Queries.BidHistory.Handle(ctx, q)
	if err != nil {
		return nil, err
	}
	return BidHistory200JSONResponse(BidHistoryPage{
		Items: toBidViews(page.Items), Total: page.Total, Page: page.Page,
	}), nil
}

func (s HTTPServer) PlaceBid(ctx context.Context, request PlaceBidRequestObject) (PlaceBidResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	bidder, err := auction.NewBidderID(user.ID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	auctionID, err := auction.NewAuctionID(request.AuctionID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	bidID, err := auction.NewBidID(request.Body.BidId)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-bid").WithCause(err)
	}
	currency, err := auction.NewCurrency(request.Body.Currency)
	if err != nil {
		return nil, errs.NewIncorrectInputError("currency-mismatch").WithCause(err)
	}
	amount, err := auction.NewMoney(request.Body.AmountMinor, currency)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-bid").WithCause(err)
	}

	if err := s.app.Commands.PlaceBid.Handle(ctx, command.PlaceBid{
		AuctionID: auctionID,
		BidID:     bidID,
		Bidder:    bidder,
		Amount:    amount,
	}); err != nil {
		return nil, err
	}

	location := "/api/auctions/" + auctionID.String() + "/bids/" + bidID.String()
	return PlaceBid204Response{
		Headers: PlaceBid204ResponseHeaders{ContentLocation: &location},
	}, nil
}

func (s HTTPServer) CancelAuction(ctx context.Context, request CancelAuctionRequestObject) (CancelAuctionResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	seller, err := auction.NewSellerID(user.ID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	auctionID, err := auction.NewAuctionID(request.AuctionID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	if err := s.app.Commands.CancelAuction.Handle(ctx, command.CancelAuction{
		AuctionID: auctionID,
		Seller:    seller,
	}); err != nil {
		return nil, err
	}
	return CancelAuction204Response{}, nil
}

func (s HTTPServer) SellerDashboard(ctx context.Context, request SellerDashboardRequestObject) (SellerDashboardResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	// Only the seller themselves: the path id must equal the
	// authenticated user — own-path-id check, nothing leaks (§8).
	if user.ID != request.SellerID {
		return nil, errs.NewForbiddenError("not-dashboard-owner")
	}
	seller, err := auction.NewSellerID(user.ID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
	}
	view, err := s.app.Queries.SellerDashboard.Handle(ctx, query.SellerDashboard{Seller: seller})
	if err != nil {
		return nil, err
	}

	items := make([]DashboardItem, 0, len(view.Items))
	for _, item := range view.Items {
		auctionID, err := uuid.Parse(item.AuctionID)
		if err != nil {
			return nil, fmt.Errorf("invalid auction id in dashboard: %w", err)
		}
		dashboardItem := DashboardItem{
			AuctionId:        auctionID,
			Title:            item.Title,
			Status:           item.Status,
			HammerPriceMinor: item.HammerPriceMinor,
			BidCount:         item.BidCount,
			EndsAt:           item.EndsAt,
		}
		if item.Outcome != "" {
			outcome := item.Outcome
			dashboardItem.Outcome = &outcome
		}
		if item.SettlementStatus != "" {
			settlement := item.SettlementStatus
			dashboardItem.SettlementStatus = &settlement
		}
		items = append(items, dashboardItem)
	}
	return SellerDashboard200JSONResponse(Dashboard{
		Items:          items,
		ActiveCount:    view.ActiveCount,
		SoldTotalMinor: view.SoldTotalMinor,
		Currency:       view.Currency,
	}), nil
}

func toBidViews(items []query.BidView) []BidView {
	views := make([]BidView, 0, len(items))
	for _, item := range items {
		view := BidView{
			BidderDisplayName: item.BidderDisplayName,
			AmountMinor:       item.AmountMinor,
			PlacedAt:          item.PlacedAt,
		}
		// Bid ids come from our own storage; a parse failure would be
		// data corruption surfaced as 500 by the error handler.
		view.BidId = uuid.MustParse(item.BidID)
		views = append(views, view)
	}
	return views
}
