package command_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"molot/internal/auction/domain/auction"
)

// Hand-written recording spies (rule 41): app tests verify
// orchestration only — which repo method ran, with which id and actor,
// and how domain errors map to slug errors.

var now0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type updateCall struct {
	id     auction.AuctionID
	actor  auction.Actor
	system bool
}

type spyRepo struct {
	auctions map[auction.AuctionID]*auction.Auction

	added   []auction.AuctionID
	updates []updateCall

	addErr error
}

func newSpyRepo() *spyRepo {
	return &spyRepo{auctions: map[auction.AuctionID]*auction.Auction{}}
}

func (r *spyRepo) seed(a *auction.Auction) {
	a.PullDomainEvents()
	r.auctions[a.ID()] = a
}

func (r *spyRepo) Add(_ context.Context, a *auction.Auction) error {
	r.added = append(r.added, a.ID())
	if r.addErr != nil {
		return r.addErr
	}
	r.auctions[a.ID()] = a
	return nil
}

func (r *spyRepo) Get(_ context.Context, id auction.AuctionID) (*auction.Auction, error) {
	a, ok := r.auctions[id]
	if !ok {
		return nil, auction.NotFoundError{AuctionID: id}
	}
	return a, nil
}

func (r *spyRepo) Update(
	ctx context.Context,
	id auction.AuctionID,
	actor auction.Actor,
	fn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	r.updates = append(r.updates, updateCall{id: id, actor: actor})
	return r.runUpdate(ctx, id, fn)
}

func (r *spyRepo) UpdateAsSystem(
	ctx context.Context,
	id auction.AuctionID,
	fn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	r.updates = append(r.updates, updateCall{id: id, system: true})
	return r.runUpdate(ctx, id, fn)
}

func (r *spyRepo) runUpdate(
	ctx context.Context,
	id auction.AuctionID,
	fn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	a, ok := r.auctions[id]
	if !ok {
		return auction.NotFoundError{AuctionID: id}
	}
	updated, err := fn(ctx, a)
	if err != nil {
		return err
	}
	updated.PullDomainEvents()
	r.auctions[id] = updated
	return nil
}

type spyProfiles struct {
	bidders map[auction.BidderID]auction.Bidder
	asked   []auction.BidderID
}

func newSpyProfiles() *spyProfiles {
	return &spyProfiles{bidders: map[auction.BidderID]auction.Bidder{}}
}

func (p *spyProfiles) BidderByID(_ context.Context, id auction.BidderID) (auction.Bidder, error) {
	p.asked = append(p.asked, id)
	if b, ok := p.bidders[id]; ok {
		return b, nil
	}
	// Contract: unknown bidder = unverified bidder.
	b, err := auction.NewBidder(id, false)
	if err != nil {
		return auction.Bidder{}, err
	}
	return b, nil
}

// --- domain fixtures via the domain API only ---

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

func newAuctionID(t *testing.T) auction.AuctionID {
	t.Helper()
	id, err := auction.NewAuctionID(uuid.New())
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
	w, err := auction.NewBiddingWindow(now0, now0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func listedAuction(t *testing.T) *auction.Auction {
	t.Helper()
	seller, err := auction.NewSellerID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	lot, err := auction.NewLot("Bronze hammer", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auction.New(newAuctionID(t), seller, lot,
		eur(t, 1000), eur(t, 100), auction.NoReserve(),
		testWindow(t), testRules(t), now0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func closedSoldAuction(t *testing.T, repo *spyRepo) *auction.Auction {
	t.Helper()
	a := listedAuction(t)
	for i, amount := range []int64{1000, 1100} {
		bidder, err := auction.NewBidder(newBidderID(t), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.PlaceBid(newBidID(t), bidder, eur(t, amount), now0.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Close(now0.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	repo.seed(a)
	return a
}
