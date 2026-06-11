package query

import (
	"context"
	"time"

	"molot/internal/auction/domain/auction"
)

// AuctionCard is the public lot page — a direct read of the write
// tables (documented decision: not every query needs a projection, §5).
type AuctionCard struct {
	AuctionID auction.AuctionID
}

type AuctionCardView struct {
	AuctionID           string
	Title               string
	Description         string
	SellerID            string
	Status              string
	Outcome             string // empty until closed
	StartPriceMinor     int64
	CurrentPriceMinor   int64
	MinimalNextBidMinor int64
	IncrementMinor      int64
	Currency            string
	HasReserve          bool // the reserve amount itself is always hidden
	StartsAt            time.Time
	EndsAt              time.Time
	ExtensionsUsed      int
	BidCount            int
	LeaderID            string // empty without bids
	RecentBids          []BidView
}

// BidView joins a bid with the bidder's display name from the
// bidder_profiles projection.
type BidView struct {
	BidID             string
	BidderDisplayName string
	AmountMinor       int64
	PlacedAt          time.Time
}

type AuctionCardReadModel interface {
	AuctionCard(ctx context.Context, id auction.AuctionID) (AuctionCardView, error)
}

type AuctionCardHandler struct {
	readModel AuctionCardReadModel
}

func NewAuctionCardHandler(readModel AuctionCardReadModel) AuctionCardHandler {
	if readModel == nil {
		panic("NewAuctionCardHandler: nil read model")
	}
	return AuctionCardHandler{readModel: readModel}
}

func (h AuctionCardHandler) Handle(ctx context.Context, q AuctionCard) (AuctionCardView, error) {
	return h.readModel.AuctionCard(ctx, q.AuctionID)
}
