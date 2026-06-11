package settlement

import (
	"errors"

	"github.com/google/uuid"
)

// Typed identities of the settlement context (validated uuid wrappers,
// §2.1). They are settlement's OWN types: contexts never import each
// other's domain (BOOK_AUDIT rule 3).

// AuctionID identifies the auction whose sale this saga settles; it is
// also the saga's primary key (one settlement per auction).
type AuctionID struct{ id uuid.UUID }

func NewAuctionID(id uuid.UUID) (AuctionID, error) {
	if id == uuid.Nil {
		return AuctionID{}, errors.New("auction id must not be nil")
	}
	return AuctionID{id: id}, nil
}

func (a AuctionID) UUID() uuid.UUID { return a.id }
func (a AuctionID) String() string  { return a.id.String() }
func (a AuctionID) IsZero() bool    { return a == AuctionID{} }

// BidderID identifies the winner or the runner-up.
type BidderID struct{ id uuid.UUID }

func NewBidderID(id uuid.UUID) (BidderID, error) {
	if id == uuid.Nil {
		return BidderID{}, errors.New("bidder id must not be nil")
	}
	return BidderID{id: id}, nil
}

func (b BidderID) UUID() uuid.UUID { return b.id }
func (b BidderID) String() string  { return b.id.String() }
func (b BidderID) IsZero() bool    { return b == BidderID{} }

// InvoiceID references the invoice the saga currently awaits. The saga
// derives it deterministically (uuidv5 of auctionID+":"+attempt, app
// layer), which makes IssueInvoice retries idempotent (§6.6).
type InvoiceID struct{ id uuid.UUID }

func NewInvoiceID(id uuid.UUID) (InvoiceID, error) {
	if id == uuid.Nil {
		return InvoiceID{}, errors.New("invoice id must not be nil")
	}
	return InvoiceID{id: id}, nil
}

func (i InvoiceID) UUID() uuid.UUID { return i.id }
func (i InvoiceID) String() string  { return i.id.String() }
func (i InvoiceID) IsZero() bool    { return i == InvoiceID{} }
