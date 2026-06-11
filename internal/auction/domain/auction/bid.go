package auction

import "time"

// Bid is an accepted bid. The aggregate keeps only the top two bids
// denormalized (leading + runner-up, ADR-0003); the full history is
// an append-only log maintained by the repository in the same
// transaction.
type Bid struct {
	id       BidID
	bidder   BidderID
	amount   Money
	placedAt time.Time
}

func NewBid(id BidID, bidder BidderID, amount Money, placedAt time.Time) (Bid, error) {
	if id.IsZero() || bidder.IsZero() || amount.IsZero() || placedAt.IsZero() {
		return Bid{}, ErrInvalidBid
	}
	return Bid{id: id, bidder: bidder, amount: amount, placedAt: placedAt}, nil
}

func (b Bid) IsZero() bool        { return b == Bid{} }
func (b Bid) ID() BidID           { return b.id }
func (b Bid) Bidder() BidderID    { return b.bidder }
func (b Bid) Amount() Money       { return b.amount }
func (b Bid) PlacedAt() time.Time { return b.placedAt }
