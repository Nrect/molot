package auction_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"molot/internal/auction/domain/auction"
)

// Fixtures are built only through the domain API (rule 40).

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func eur(t *testing.T, amount int64) auction.Money {
	t.Helper()
	c, err := auction.NewCurrency("EUR")
	if err != nil {
		t.Fatal(err)
	}
	m, err := auction.NewMoney(amount, c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func usd(t *testing.T, amount int64) auction.Money {
	t.Helper()
	c, err := auction.NewCurrency("USD")
	if err != nil {
		t.Fatal(err)
	}
	m, err := auction.NewMoney(amount, c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newAuctionID(t *testing.T) auction.AuctionID {
	t.Helper()
	id, err := auction.NewAuctionID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newSellerID(t *testing.T) auction.SellerID {
	t.Helper()
	id, err := auction.NewSellerID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newBidderID(t *testing.T) auction.BidderID {
	t.Helper()
	id, err := auction.NewBidderID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newBidID(t *testing.T) auction.BidID {
	t.Helper()
	id, err := auction.NewBidID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func verifiedBidder(t *testing.T) auction.Bidder {
	t.Helper()
	b, err := auction.NewBidder(newBidderID(t), true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unverifiedBidder(t *testing.T) auction.Bidder {
	t.Helper()
	b, err := auction.NewBidder(newBidderID(t), false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testLot(t *testing.T) auction.Lot {
	t.Helper()
	lot, err := auction.NewLot("Bronze hammer", "a fine bronze hammer")
	if err != nil {
		t.Fatal(err)
	}
	return lot
}

// testRules: platform currency EUR, verification required at >= 100_000,
// anti-snipe: window 5m, extension 10m, max 2 extensions.
func testRules(t *testing.T) auction.ListingRules {
	t.Helper()
	c, err := auction.NewCurrency("EUR")
	if err != nil {
		t.Fatal(err)
	}
	snipe, err := auction.NewAntiSnipePolicy(5*time.Minute, 10*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := auction.NewListingRules(c, eur(t, 100_000), snipe)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func testWindow(t *testing.T) auction.BiddingWindow {
	t.Helper()
	w, err := auction.NewBiddingWindow(t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// listedAuction is the canonical open auction: start 1000, increment
// 100, no reserve, window [t0, t0+24h), listed at t0.
func listedAuction(t *testing.T) *auction.Auction {
	t.Helper()
	a, err := auction.New(
		newAuctionID(t), newSellerID(t), testLot(t),
		eur(t, 1000), eur(t, 100), auction.NoReserve(),
		testWindow(t), testRules(t), t0,
	)
	if err != nil {
		t.Fatal(err)
	}
	a.PullDomainEvents() // start tests from a clean slate
	return a
}

// listedAuctionWithReserve is listedAuction with a reserve price.
func listedAuctionWithReserve(t *testing.T, reserveMinor int64) *auction.Auction {
	t.Helper()
	reserve, err := auction.NewReservePrice(eur(t, reserveMinor))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auction.New(
		newAuctionID(t), newSellerID(t), testLot(t),
		eur(t, 1000), eur(t, 100), reserve,
		testWindow(t), testRules(t), t0,
	)
	if err != nil {
		t.Fatal(err)
	}
	a.PullDomainEvents()
	return a
}

// placeBid places a bid that must succeed.
func placeBid(t *testing.T, a *auction.Auction, b auction.Bidder, amount auction.Money, now time.Time) auction.Bid {
	t.Helper()
	bid, err := a.PlaceBid(newBidID(t), b, amount, now)
	if err != nil {
		t.Fatalf("placeBid: %v", err)
	}
	return bid
}

// closedSoldAuction returns an auction closed as sold with a leading
// and a runner-up bid, plus both bidders.
func closedSoldAuction(t *testing.T) (*auction.Auction, auction.Bid, auction.Bid) {
	t.Helper()
	a := listedAuction(t)
	first := verifiedBidder(t)
	second := verifiedBidder(t)
	runnerUpBid := placeBid(t, a, first, eur(t, 1000), t0.Add(time.Hour))
	winnerBid := placeBid(t, a, second, eur(t, 1100), t0.Add(2*time.Hour))
	if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	a.PullDomainEvents()
	return a, winnerBid, runnerUpBid
}
