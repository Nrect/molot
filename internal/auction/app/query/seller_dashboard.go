package query

import (
	"context"
	"time"

	"molot/internal/auction/domain/auction"
)

// SellerDashboard is the seller's own overview, backed by the
// seller_dashboard_items projection. Only the seller themselves may
// read it — the port matches the path id against the authenticated
// user before building the query (own-path-id, no leak, §8).
type SellerDashboard struct {
	Seller auction.SellerID
}

type DashboardView struct {
	Items       []DashboardItem
	ActiveCount int
	// SoldTotalMinor is a sound single-currency aggregation: the
	// platform currency is unique (PLATFORM_CURRENCY, guarded at
	// listing time).
	SoldTotalMinor int64
	Currency       string
}

type DashboardItem struct {
	AuctionID        string
	Title            string
	Status           string
	Outcome          string
	SettlementStatus string
	HammerPriceMinor int64
	BidCount         int
	EndsAt           time.Time
}

type SellerDashboardReadModel interface {
	Dashboard(ctx context.Context, seller auction.SellerID) (DashboardView, error)
}

type SellerDashboardHandler struct {
	readModel SellerDashboardReadModel
}

func NewSellerDashboardHandler(readModel SellerDashboardReadModel) SellerDashboardHandler {
	if readModel == nil {
		panic("NewSellerDashboardHandler: nil read model")
	}
	return SellerDashboardHandler{readModel: readModel}
}

func (h SellerDashboardHandler) Handle(ctx context.Context, q SellerDashboard) (DashboardView, error) {
	return h.readModel.Dashboard(ctx, q.Seller)
}
