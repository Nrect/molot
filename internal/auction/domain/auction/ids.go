package auction

import "github.com/google/uuid"

// Typed IDs are uuid wrappers with validating constructors, so a
// BidderID can never be passed where a SellerID is expected.

// AuctionID identifies an Auction aggregate.
type AuctionID struct {
	id uuid.UUID
}

func NewAuctionID(id uuid.UUID) (AuctionID, error) {
	if id == uuid.Nil {
		return AuctionID{}, ErrInvalidID
	}
	return AuctionID{id: id}, nil
}

func (i AuctionID) IsZero() bool    { return i == AuctionID{} }
func (i AuctionID) UUID() uuid.UUID { return i.id }
func (i AuctionID) String() string  { return i.id.String() }

// SellerID identifies the participant selling a lot.
type SellerID struct {
	id uuid.UUID
}

func NewSellerID(id uuid.UUID) (SellerID, error) {
	if id == uuid.Nil {
		return SellerID{}, ErrInvalidID
	}
	return SellerID{id: id}, nil
}

func (i SellerID) IsZero() bool    { return i == SellerID{} }
func (i SellerID) UUID() uuid.UUID { return i.id }
func (i SellerID) String() string  { return i.id.String() }

// BidderID identifies a participant placing bids.
type BidderID struct {
	id uuid.UUID
}

func NewBidderID(id uuid.UUID) (BidderID, error) {
	if id == uuid.Nil {
		return BidderID{}, ErrInvalidID
	}
	return BidderID{id: id}, nil
}

func (i BidderID) IsZero() bool    { return i == BidderID{} }
func (i BidderID) UUID() uuid.UUID { return i.id }
func (i BidderID) String() string  { return i.id.String() }

// BidID identifies a single bid (client-generated, ARCHITECTURE §8).
type BidID struct {
	id uuid.UUID
}

func NewBidID(id uuid.UUID) (BidID, error) {
	if id == uuid.Nil {
		return BidID{}, ErrInvalidID
	}
	return BidID{id: id}, nil
}

func (i BidID) IsZero() bool    { return i == BidID{} }
func (i BidID) UUID() uuid.UUID { return i.id }
func (i BidID) String() string  { return i.id.String() }
