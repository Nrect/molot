// Package query holds the read use cases of the auction context.
// Results are UI-shaped — not domain, not OpenAPI, not DB models
// (rule 26). Each read-model interface is declared next to its query
// handler (consumer side); the storage behind it is transparent
// (rule 32).
package query

import (
	"context"
	"time"
)

// ActiveAuctionsCatalog pages through the public catalog of open
// auctions, backed by the catalog_items projection.
type ActiveAuctionsCatalog struct {
	Page     int
	PageSize int
}

type CatalogPage struct {
	Items []CatalogItem
	Total int
	Page  int
}

type CatalogItem struct {
	AuctionID           string
	Title               string
	SellerID            string
	CurrentPriceMinor   int64
	MinimalNextBidMinor int64
	Currency            string
	BidCount            int
	EndsAt              time.Time
	// EndingSoon is computed by the adapter at read time
	// (ends_at - now() < 5 min) — never stored, it would go stale
	// between events (§5).
	EndingSoon bool
}

type ActiveCatalogReadModel interface {
	ActiveAuctions(ctx context.Context, page, pageSize int) (CatalogPage, error)
}

type ActiveAuctionsCatalogHandler struct {
	readModel ActiveCatalogReadModel
}

func NewActiveAuctionsCatalogHandler(readModel ActiveCatalogReadModel) ActiveAuctionsCatalogHandler {
	if readModel == nil {
		panic("NewActiveAuctionsCatalogHandler: nil read model")
	}
	return ActiveAuctionsCatalogHandler{readModel: readModel}
}

func (h ActiveAuctionsCatalogHandler) Handle(ctx context.Context, q ActiveAuctionsCatalog) (CatalogPage, error) {
	page, pageSize := normalizePaging(q.Page, q.PageSize)
	return h.readModel.ActiveAuctions(ctx, page, pageSize)
}

// normalizePaging clamps paging input to sane presentation defaults.
func normalizePaging(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
