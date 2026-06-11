package auction_test

import (
	"errors"
	"testing"
	"time"

	"molot/internal/auction/domain/auction"
)

func TestAuction_PlaceBid(t *testing.T) {
	t.Parallel()

	bidTime := t0.Add(time.Hour)

	tests := []struct {
		name string
		// setup returns the aggregate and the bidder placing the bid
		setup   func(t *testing.T) (*auction.Auction, auction.Bidder)
		amount  int64 // EUR minor units; 0 means "use custom money"
		money   func(t *testing.T) auction.Money
		now     time.Time
		wantErr error
	}{
		{
			name: "first bid at start price is accepted",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 1000, now: bidTime,
		},
		{
			name: "first bid above start price is accepted",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 5000, now: bidTime,
		},
		{
			name: "first bid below start price is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 999, now: bidTime, wantErr: auction.ErrBidBelowMinimum,
		},
		{
			name: "second bid at leading+increment is accepted",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				placeBid(t, a, verifiedBidder(t), eur(t, 1000), bidTime)
				return a, verifiedBidder(t)
			},
			amount: 1100, now: bidTime.Add(time.Minute),
		},
		{
			name: "second bid below leading+increment is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				placeBid(t, a, verifiedBidder(t), eur(t, 1000), bidTime)
				return a, verifiedBidder(t)
			},
			amount: 1099, now: bidTime.Add(time.Minute), wantErr: auction.ErrBidBelowMinimum,
		},
		{
			name: "bid equal to current leading is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				placeBid(t, a, verifiedBidder(t), eur(t, 1000), bidTime)
				return a, verifiedBidder(t)
			},
			amount: 1000, now: bidTime.Add(time.Minute), wantErr: auction.ErrBidBelowMinimum,
		},
		{
			name: "seller cannot bid on own auction",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				sellerAsBidder, err := auction.NewBidderID(a.Seller().UUID())
				if err != nil {
					t.Fatal(err)
				}
				b, err := auction.NewBidder(sellerAsBidder, true)
				if err != nil {
					t.Fatal(err)
				}
				return a, b
			},
			amount: 1000, now: bidTime, wantErr: auction.ErrSellerCannotBid,
		},
		{
			name: "leader cannot outbid self",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				leader := verifiedBidder(t)
				placeBid(t, a, leader, eur(t, 1000), bidTime)
				return a, leader
			},
			amount: 1100, now: bidTime.Add(time.Minute), wantErr: auction.ErrLeaderCannotOutbidSelf,
		},
		{
			name: "currency mismatch is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			money: func(t *testing.T) auction.Money { return usd(t, 5000) },
			now:   bidTime, wantErr: auction.ErrCurrencyMismatch,
		},
		{
			name: "bid before window opens is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 1000, now: t0.Add(-time.Minute), wantErr: auction.ErrAuctionNotOpen,
		},
		{
			name: "bid after window closes is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 1000, now: t0.Add(24 * time.Hour), wantErr: auction.ErrAuctionNotOpen,
		},
		{
			name: "bid exactly at window end is rejected (end exclusive)",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 1000, now: t0.Add(24*time.Hour - time.Nanosecond),
		},
		{
			name: "bid on cancelled auction is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				if err := a.Cancel(a.Seller(), bidTime); err != nil {
					t.Fatal(err)
				}
				return a, verifiedBidder(t)
			},
			amount: 1000, now: bidTime, wantErr: auction.ErrAuctionNotOpen,
		},
		{
			name: "bid on closed auction is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
					t.Fatal(err)
				}
				return a, verifiedBidder(t)
			},
			amount: 1000, now: t0.Add(26 * time.Hour), wantErr: auction.ErrAuctionNotOpen,
		},
		{
			name: "unverified bidder above threshold is rejected",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), unverifiedBidder(t)
			},
			amount: 100_001, now: bidTime, wantErr: auction.ErrVerificationRequired,
		},
		{
			name: "unverified bidder exactly at threshold is rejected (GTE)",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), unverifiedBidder(t)
			},
			amount: 100_000, now: bidTime, wantErr: auction.ErrVerificationRequired,
		},
		{
			name: "unverified bidder below threshold is accepted",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), unverifiedBidder(t)
			},
			amount: 99_999, now: bidTime,
		},
		{
			name: "verified bidder above threshold is accepted",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), verifiedBidder(t)
			},
			amount: 500_000, now: bidTime,
		},
		{
			name: "guard order: closed auction wins over below-minimum",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
					t.Fatal(err)
				}
				return a, verifiedBidder(t)
			},
			amount: 1, now: t0.Add(26 * time.Hour), wantErr: auction.ErrAuctionNotOpen,
		},
		{
			name: "guard order: seller wins over below-minimum",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				a := listedAuction(t)
				sellerAsBidder, err := auction.NewBidderID(a.Seller().UUID())
				if err != nil {
					t.Fatal(err)
				}
				b, err := auction.NewBidder(sellerAsBidder, true)
				if err != nil {
					t.Fatal(err)
				}
				return a, b
			},
			amount: 1, now: bidTime, wantErr: auction.ErrSellerCannotBid,
		},
		{
			name: "guard order: currency mismatch wins over verification",
			setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
				return listedAuction(t), unverifiedBidder(t)
			},
			money: func(t *testing.T) auction.Money { return usd(t, 500_000) },
			now:   bidTime, wantErr: auction.ErrCurrencyMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, bidder := tt.setup(t)
			amount := tt.money
			if amount == nil {
				amount = func(t *testing.T) auction.Money { return eur(t, tt.amount) }
			}
			prevCount := a.BidCount()

			bid, err := a.PlaceBid(newBidID(t), bidder, amount(t), tt.now)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("PlaceBid error = %v, want %v", err, tt.wantErr)
				}
				if a.BidCount() != prevCount {
					t.Fatalf("rejected bid mutated bidCount: %d -> %d", prevCount, a.BidCount())
				}
				return
			}
			if err != nil {
				t.Fatalf("PlaceBid: %v", err)
			}
			if bid.IsZero() {
				t.Fatal("accepted bid is zero")
			}
			if got := a.LeadingBid(); got != bid {
				t.Fatalf("leading bid = %+v, want the accepted bid", got)
			}
			if a.BidCount() != prevCount+1 {
				t.Fatalf("bidCount = %d, want %d", a.BidCount(), prevCount+1)
			}
		})
	}
}

func TestAuction_PlaceBid_PromotesRunnerUp(t *testing.T) {
	t.Parallel()
	a := listedAuction(t)
	first := verifiedBidder(t)
	second := verifiedBidder(t)
	third := verifiedBidder(t)

	bid1 := placeBid(t, a, first, eur(t, 1000), t0.Add(time.Hour))
	if !a.RunnerUpBid().IsZero() {
		t.Fatal("runner-up after first bid must be zero")
	}

	bid2 := placeBid(t, a, second, eur(t, 1100), t0.Add(2*time.Hour))
	if a.RunnerUpBid() != bid1 {
		t.Fatal("first bid must become runner-up after second bid")
	}

	bid3 := placeBid(t, a, third, eur(t, 1200), t0.Add(3*time.Hour))
	if a.RunnerUpBid() != bid2 {
		t.Fatal("second bid must become runner-up after third bid")
	}
	if a.LeadingBid() != bid3 {
		t.Fatal("third bid must lead")
	}
	if a.BidCount() != 3 {
		t.Fatalf("bidCount = %d, want 3", a.BidCount())
	}
}

func TestAuction_PlaceBid_RecordsBidPlacedEvent(t *testing.T) {
	t.Parallel()
	a := listedAuction(t)
	first := verifiedBidder(t)
	second := verifiedBidder(t)

	bid1 := placeBid(t, a, first, eur(t, 1000), t0.Add(time.Hour))
	a.PullDomainEvents()
	bid2 := placeBid(t, a, second, eur(t, 1100), t0.Add(2*time.Hour))

	events := a.PullDomainEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	want := auction.BidPlaced{
		Bid:        bid2,
		Outbid:     bid1,
		NewEndsAt:  a.EndsAt(),
		Extended:   false,
		OccurredAt: t0.Add(2 * time.Hour),
	}
	// All BidPlaced fields are comparable value objects, so direct
	// equality is an exact structural comparison.
	if events[0] != auction.DomainEvent(want) {
		t.Fatalf("BidPlaced mismatch:\n got %+v\nwant %+v", events[0], want)
	}

	if pulled := a.PullDomainEvents(); len(pulled) != 0 {
		t.Fatalf("second drain returned %d events, want 0", len(pulled))
	}
}

func TestAuction_MinimalNextBid(t *testing.T) {
	t.Parallel()
	a := listedAuction(t)
	if got := auction.MinimalNextBid(*a); got != eur(t, 1000) {
		t.Fatalf("MinimalNextBid without bids = %v, want start price", got)
	}
	placeBid(t, a, verifiedBidder(t), eur(t, 2000), t0.Add(time.Hour))
	if got := auction.MinimalNextBid(*a); got != eur(t, 2100) {
		t.Fatalf("MinimalNextBid = %v, want leading+increment", got)
	}
}
