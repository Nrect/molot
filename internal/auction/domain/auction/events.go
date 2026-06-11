package auction

import "time"

// DomainEvent is the closed set of rich internal events recorded by
// behavior methods and drained by the repository adapter, which maps
// them to flat integration events and publishes through the outbox in
// the same transaction as the aggregate persist (§4.1).
type DomainEvent interface {
	isDomainEvent()
}

// AuctionListed — recorded by New and RelistedFrom.
type AuctionListed struct {
	OccurredAt time.Time
}

// BidPlaced — recorded by PlaceBid. Outbid is the previous leading bid
// (zero for the first bid); Extended reports an anti-snipe extension.
type BidPlaced struct {
	Bid        Bid
	Outbid     Bid
	NewEndsAt  time.Time
	Extended   bool
	OccurredAt time.Time
}

// AuctionExtended — recorded on an anti-snipe extension. Domain-only:
// it has no integration mapping (BidPlacedV1 carries Extended+NewEndsAt).
type AuctionExtended struct {
	NewEndsAt  time.Time
	OccurredAt time.Time
}

// AuctionCancelled — recorded by Cancel.
type AuctionCancelled struct {
	OccurredAt time.Time
}

// AuctionClosed — recorded by Close with the full closing verdict.
type AuctionClosed struct {
	Result     ClosingResult
	OccurredAt time.Time
}

// WinnerReassigned — recorded by AwardToRunnerUp, exactly once per
// auction (guarded by the winnerReassigned flag).
type WinnerReassigned struct {
	NewWinner  Bid
	OccurredAt time.Time
}

// AuctionRelisted — recorded by RelistedFrom on the NEW aggregate,
// referencing the original it replaces.
type AuctionRelisted struct {
	OriginalID AuctionID
	NewID      AuctionID
	StartsAt   time.Time
	EndsAt     time.Time
	OccurredAt time.Time
}

// SaleSettled — recorded by ConfirmSettlement.
type SaleSettled struct {
	OccurredAt time.Time
}

// SaleFailed — recorded by FailSale with the saga's failure reason.
type SaleFailed struct {
	Reason     FailureReason
	OccurredAt time.Time
}

func (AuctionListed) isDomainEvent()    {}
func (BidPlaced) isDomainEvent()        {}
func (AuctionExtended) isDomainEvent()  {}
func (AuctionCancelled) isDomainEvent() {}
func (AuctionClosed) isDomainEvent()    {}
func (WinnerReassigned) isDomainEvent() {}
func (AuctionRelisted) isDomainEvent()  {}
func (SaleSettled) isDomainEvent()      {}
func (SaleFailed) isDomainEvent()       {}
