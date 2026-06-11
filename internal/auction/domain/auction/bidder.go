package auction

// Bidder is the bidding participant as the auction context knows it —
// hydrated from the local bidder_profiles projection of participant
// events, never from the participant context directly.
type Bidder struct {
	id       BidderID
	verified bool
}

func NewBidder(id BidderID, verified bool) (Bidder, error) {
	if id.IsZero() {
		return Bidder{}, ErrInvalidID
	}
	return Bidder{id: id, verified: verified}, nil
}

func (b Bidder) IsZero() bool   { return b == Bidder{} }
func (b Bidder) ID() BidderID   { return b.id }
func (b Bidder) Verified() bool { return b.verified }
