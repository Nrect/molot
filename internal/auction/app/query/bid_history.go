package query

import (
	"context"

	"molot/internal/auction/domain/auction"
)

// BidHistory pages through the append-only bid log of one auction —
// a direct read of auction_bids, which is already a perfect history (§5).
type BidHistory struct {
	AuctionID auction.AuctionID
	Page      int
	PageSize  int
}

type BidHistoryPage struct {
	Items []BidView
	Total int
	Page  int
}

type BidHistoryReadModel interface {
	Bids(ctx context.Context, id auction.AuctionID, page, pageSize int) (BidHistoryPage, error)
}

type BidHistoryHandler struct {
	readModel BidHistoryReadModel
}

func NewBidHistoryHandler(readModel BidHistoryReadModel) BidHistoryHandler {
	if readModel == nil {
		panic("NewBidHistoryHandler: nil read model")
	}
	return BidHistoryHandler{readModel: readModel}
}

func (h BidHistoryHandler) Handle(ctx context.Context, q BidHistory) (BidHistoryPage, error) {
	page, pageSize := normalizePaging(q.Page, q.PageSize)
	return h.readModel.Bids(ctx, q.AuctionID, page, pageSize)
}
