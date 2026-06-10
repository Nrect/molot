// Package events defines the auction context's public integration
// events — the ONLY auction package other contexts may import.
//
// Events are flat, versioned and append-only: a breaking change means a
// new V2 struct dual-published next to V1, never a field edit. Payloads
// carry primitives and time.Time only; all timestamps are UTC.
package events

import "time"

// Topic is the watermill topic auction integration events are published to.
const Topic = "auction-events"

type AuctionListedV1 struct {
	EventID          string    `json:"event_id"`
	AuctionID        string    `json:"auction_id"`
	SellerID         string    `json:"seller_id"`
	Title            string    `json:"title"`
	StartPriceMinor  int64     `json:"start_price_minor"`
	Currency         string    `json:"currency"`
	StartsAt         time.Time `json:"starts_at"`
	EndsAt           time.Time `json:"ends_at"`
	RelistGeneration int       `json:"relist_generation"`
	OccurredAt       time.Time `json:"occurred_at"`
}

type BidPlacedV1 struct {
	EventID        string    `json:"event_id"`
	AuctionID      string    `json:"auction_id"`
	BidID          string    `json:"bid_id"`
	BidderID       string    `json:"bidder_id"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	BidCount       int       `json:"bid_count"`
	Extended       bool      `json:"extended"`
	NewEndsAt      time.Time `json:"new_ends_at"`
	OutbidBidderID string    `json:"outbid_bidder_id"` // empty if this is the first bid
	OccurredAt     time.Time `json:"occurred_at"`
}

type AuctionCancelledV1 struct {
	EventID    string    `json:"event_id"`
	AuctionID  string    `json:"auction_id"`
	SellerID   string    `json:"seller_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// AuctionClosedV1 never exposes the reserve price: subscribers get the
// ready-made RunnerUpQualifies verdict instead.
type AuctionClosedV1 struct {
	EventID             string    `json:"event_id"`
	AuctionID           string    `json:"auction_id"`
	SellerID            string    `json:"seller_id"`
	Outcome             string    `json:"outcome"` // sold | not_sold
	WinnerID            string    `json:"winner_id"`
	HammerPriceMinor    int64     `json:"hammer_price_minor"`
	Currency            string    `json:"currency"`
	RunnerUpBidderID    string    `json:"runner_up_bidder_id"`
	RunnerUpAmountMinor int64     `json:"runner_up_amount_minor"`
	RunnerUpQualifies   bool      `json:"runner_up_qualifies"`
	RelistGeneration    int       `json:"relist_generation"`
	OccurredAt          time.Time `json:"occurred_at"`
}

type WinnerReassignedV1 struct {
	EventID     string    `json:"event_id"`
	AuctionID   string    `json:"auction_id"`
	NewWinnerID string    `json:"new_winner_id"`
	PriceMinor  int64     `json:"price_minor"`
	Currency    string    `json:"currency"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type AuctionRelistedV1 struct {
	EventID           string    `json:"event_id"`
	OriginalAuctionID string    `json:"original_auction_id"`
	NewAuctionID      string    `json:"new_auction_id"`
	StartsAt          time.Time `json:"starts_at"`
	EndsAt            time.Time `json:"ends_at"`
	OccurredAt        time.Time `json:"occurred_at"`
}

type SaleSettledV1 struct {
	EventID    string    `json:"event_id"`
	AuctionID  string    `json:"auction_id"`
	WinnerID   string    `json:"winner_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

type SaleFailedV1 struct {
	EventID    string    `json:"event_id"`
	AuctionID  string    `json:"auction_id"`
	Reason     string    `json:"reason"` // payment_timeout | second_chance_declined
	OccurredAt time.Time `json:"occurred_at"`
}
