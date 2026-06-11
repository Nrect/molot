package adapters_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"molot/internal/auction/domain/auction"
)

// One shared suite runs against every Repository implementation
// (rule 42): the in-memory adapter always, the postgres adapter under
// the integration build tag. Unique data instead of cleanup; asserts
// by concrete id; t.Parallel everywhere.

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// testRepository is what the suite needs: the domain Repository plus
// the worker's candidate scan.
type testRepository interface {
	auction.Repository
	DueForClosing(ctx context.Context, before time.Time, limit int) ([]auction.AuctionID, error)
}

func runRepositorySuite(t *testing.T, newRepo func(t *testing.T) testRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("add and get roundtrip", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t, withReserveFx(t, 5000))

		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Get(ctx, a.ID())
		if err != nil {
			t.Fatal(err)
		}
		assertSameAuction(t, a, got)
		if got.Version() != 1 {
			t.Fatalf("version = %d, want 1", got.Version())
		}
	})

	t.Run("add is idempotent for the same id", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t)
		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}
		retry := listedAuctionFx(t) // same id, fresh aggregate
		retry = rebuildWithID(t, retry, a.ID())
		if err := repo.Add(ctx, retry); err != nil {
			t.Fatalf("retried add: %v, want nil", err)
		}
		got, err := repo.Get(ctx, a.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.Seller() != a.Seller() {
			t.Fatal("retry must not overwrite the original row")
		}
	})

	t.Run("second relist of the same original returns ErrAlreadyRelisted", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		orig := closedSoldAuctionFx(t)
		if err := repo.Add(ctx, orig); err != nil {
			t.Fatal(err)
		}
		window, err := auction.NewBiddingWindow(t0.Add(30*time.Hour), t0.Add(54*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		first, err := auction.RelistedFrom(orig, newAuctionIDFx(t), window, t0.Add(29*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Add(ctx, first); err != nil {
			t.Fatal(err)
		}
		// A different new id for the same original — only UNIQUE(relist_of)
		// can catch it.
		second, err := auction.RelistedFrom(orig, newAuctionIDFx(t), window, t0.Add(29*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Add(ctx, second); !errors.Is(err, auction.ErrAlreadyRelisted) {
			t.Fatalf("err = %v, want ErrAlreadyRelisted", err)
		}
	})

	t.Run("get missing returns NotFoundError", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		missing := newAuctionIDFx(t)
		_, err := repo.Get(ctx, missing)
		var notFound auction.NotFoundError
		if !errors.As(err, &notFound) || notFound.AuctionID != missing {
			t.Fatalf("err = %v, want NotFoundError with the id", err)
		}
	})

	t.Run("update persists the closure result and bumps the version", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t)
		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}
		bidder := verifiedBidderFx(t)
		bidID := newBidIDFx(t)

		err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				if _, err := current.PlaceBid(bidID, bidder, eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
					return nil, err
				}
				return current, nil
			})
		if err != nil {
			t.Fatal(err)
		}

		got, err := repo.Get(ctx, a.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.BidCount() != 1 || got.LeadingBid().ID() != bidID {
			t.Fatalf("bid not persisted: count=%d leading=%v", got.BidCount(), got.LeadingBid().ID())
		}
		if got.Version() != 2 {
			t.Fatalf("version = %d, want 2", got.Version())
		}
	})

	t.Run("update rolls back when the closure fails", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t)
		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}
		sentinel := errors.New("sabotage")

		err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(newBidderIDFx(t)),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				// Mutate first, then fail: nothing may leak out.
				if _, err := current.PlaceBid(newBidIDFx(t), verifiedBidderFx(t), eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
					return nil, err
				}
				return nil, sentinel
			})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want the closure error", err)
		}

		got, err := repo.Get(ctx, a.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.BidCount() != 0 || got.Version() != 1 {
			t.Fatalf("rollback leaked: count=%d version=%d", got.BidCount(), got.Version())
		}
	})

	t.Run("update without an actor is rejected; UpdateAsSystem works", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t)
		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}
		err := repo.Update(ctx, a.ID(), auction.Actor{},
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				return current, nil
			})
		if err == nil {
			t.Fatal("zero actor must be rejected")
		}
		err = repo.UpdateAsSystem(ctx, a.ID(),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				if _, err := current.Close(t0.Add(25 * time.Hour)); err != nil {
					return nil, err
				}
				return current, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := repo.Get(ctx, a.ID())
		if got.Status() != auction.StatusClosed {
			t.Fatal("system update must persist")
		}
	})

	t.Run("update on a missing auction returns NotFoundError", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		var notFound auction.NotFoundError
		err := repo.UpdateAsSystem(ctx, newAuctionIDFx(t),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				return current, nil
			})
		if !errors.As(err, &notFound) {
			t.Fatalf("err = %v, want NotFoundError", err)
		}
	})

	t.Run("DueForClosing returns only due listed auctions", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		due := listedAuctionFx(t) // ends t0+24h
		notDue := listedAuctionFx(t)
		closed := closedSoldAuctionFx(t)
		for _, a := range []*auction.Auction{due, notDue, closed} {
			if err := repo.Add(ctx, a); err != nil {
				t.Fatal(err)
			}
		}

		ids, err := repo.DueForClosing(ctx, t0.Add(25*time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		found := map[auction.AuctionID]bool{}
		for _, id := range ids {
			found[id] = true
		}
		// Both open fixtures end at t0+24h: each must be due; the
		// closed one must not. Other tests' auctions may also appear —
		// assert by concrete ids only (rule 42).
		if !found[due.ID()] || !found[notDue.ID()] {
			t.Fatal("due listed auctions missing from the scan")
		}
		if found[closed.ID()] {
			t.Fatal("closed auction must not be scanned")
		}

		ids, err = repo.DueForClosing(ctx, t0.Add(23*time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if id == due.ID() {
				t.Fatal("not-yet-due auction must not be scanned")
			}
		}
	})

	t.Run("race: 20 concurrent equal bids, exactly one winner", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t)
		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}

		const bidders = 20
		start := make(chan struct{})
		winners := make(chan auction.BidID, bidders)
		var wg sync.WaitGroup

		for i := 0; i < bidders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				bidder := verifiedBidderFx(t)
				bidID := newBidIDFx(t)
				<-start
				err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
					func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
						if _, err := current.PlaceBid(bidID, bidder, eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
							return nil, err
						}
						return current, nil
					})
				if err == nil {
					winners <- bidID
				} else if !errors.Is(err, auction.ErrBidBelowMinimum) {
					t.Errorf("loser got unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(winners)

		var won []auction.BidID
		for id := range winners {
			won = append(won, id)
		}
		if len(won) != 1 {
			t.Fatalf("winners = %d, want exactly 1", len(won))
		}
		got, err := repo.Get(ctx, a.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.BidCount() != 1 || got.LeadingBid().ID() != won[0] {
			t.Fatalf("final state: count=%d leading=%v, want the single winner", got.BidCount(), got.LeadingBid().ID())
		}
		if got.Version() != 2 {
			t.Fatalf("version = %d, want 2 (one successful update)", got.Version())
		}
	})

	t.Run("race: close vs snipe bid — one consistent outcome", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		a := listedAuctionFx(t)
		if err := repo.Add(ctx, a); err != nil {
			t.Fatal(err)
		}
		deadline := t0.Add(24 * time.Hour)
		bidder := verifiedBidderFx(t)

		start := make(chan struct{})
		var closeErr, bidErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			closeErr = repo.UpdateAsSystem(ctx, a.ID(),
				func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
					if _, err := current.Close(deadline); err != nil {
						return nil, err
					}
					return current, nil
				})
		}()
		go func() {
			defer wg.Done()
			<-start
			bidErr = repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
				func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
					// Inside the snipe window (5m before the deadline).
					if _, err := current.PlaceBid(newBidIDFx(t), bidder, eurFx(t, 1000), deadline.Add(-time.Minute)); err != nil {
						return nil, err
					}
					return current, nil
				})
		}()
		close(start)
		wg.Wait()

		got, err := repo.Get(ctx, a.ID())
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case bidErr == nil && errors.Is(closeErr, auction.ErrBiddingStillOpen):
			// Bid won: the window extended, the close was refused.
			if got.Status() != auction.StatusListed || got.BidCount() != 1 {
				t.Fatalf("bid-won state inconsistent: %v/%d", got.Status(), got.BidCount())
			}
			if !got.EndsAt().After(deadline) {
				t.Fatal("snipe bid must have extended the deadline")
			}
		case closeErr == nil && errors.Is(bidErr, auction.ErrAuctionNotOpen):
			// Close won: the late bid was refused.
			if got.Status() != auction.StatusClosed || got.BidCount() != 0 {
				t.Fatalf("close-won state inconsistent: %v/%d", got.Status(), got.BidCount())
			}
		default:
			t.Fatalf("no consistent winner: closeErr=%v bidErr=%v", closeErr, bidErr)
		}
	})
}

// --- fixtures (domain API only) ---------------------------------------

func eurFx(t *testing.T, amount int64) auction.Money {
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

func newAuctionIDFx(t *testing.T) auction.AuctionID {
	t.Helper()
	id, err := auction.NewAuctionID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newBidderIDFx(t *testing.T) auction.BidderID {
	t.Helper()
	id, err := auction.NewBidderID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newBidIDFx(t *testing.T) auction.BidID {
	t.Helper()
	id, err := auction.NewBidID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func verifiedBidderFx(t *testing.T) auction.Bidder {
	t.Helper()
	b, err := auction.NewBidder(newBidderIDFx(t), true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fxOption func(t *testing.T, args *fxArgs)

type fxArgs struct {
	reserve auction.ReservePrice
}

func withReserveFx(t *testing.T, minor int64) fxOption {
	return func(t *testing.T, args *fxArgs) {
		r, err := auction.NewReservePrice(eurFx(t, minor))
		if err != nil {
			t.Fatal(err)
		}
		args.reserve = r
	}
}

// listedAuctionFx: start 1000, increment 100, window [t0, t0+24h),
// snipe 5m/+10m max 2, verification at >= 100000.
func listedAuctionFx(t *testing.T, opts ...fxOption) *auction.Auction {
	t.Helper()
	args := fxArgs{reserve: auction.NoReserve()}
	for _, opt := range opts {
		opt(t, &args)
	}
	currency, err := auction.NewCurrency("EUR")
	if err != nil {
		t.Fatal(err)
	}
	snipe, err := auction.NewAntiSnipePolicy(5*time.Minute, 10*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := auction.NewListingRules(currency, eurFx(t, 100_000), snipe)
	if err != nil {
		t.Fatal(err)
	}
	seller, err := auction.NewSellerID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	lot, err := auction.NewLot("Bronze hammer", "a fine bronze hammer")
	if err != nil {
		t.Fatal(err)
	}
	window, err := auction.NewBiddingWindow(t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auction.New(newAuctionIDFx(t), seller, lot,
		eurFx(t, 1000), eurFx(t, 100), args.reserve, window, rules, t0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func closedSoldAuctionFx(t *testing.T) *auction.Auction {
	t.Helper()
	a := listedAuctionFx(t)
	for i, amount := range []int64{1000, 1100} {
		bidder := verifiedBidderFx(t)
		if _, err := a.PlaceBid(newBidIDFx(t), bidder, eurFx(t, amount), t0.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	return a
}

// rebuildWithID re-creates an aggregate with a forced id through the
// domain factory (fixtures only via the domain API).
func rebuildWithID(t *testing.T, src *auction.Auction, id auction.AuctionID) *auction.Auction {
	t.Helper()
	seller, err := auction.NewSellerID(uuid.New()) // different seller proves no overwrite
	if err != nil {
		t.Fatal(err)
	}
	currency, err := auction.NewCurrency("EUR")
	if err != nil {
		t.Fatal(err)
	}
	snipe, err := auction.NewAntiSnipePolicy(5*time.Minute, 10*time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := auction.NewListingRules(currency, eurFx(t, 100_000), snipe)
	if err != nil {
		t.Fatal(err)
	}
	window, err := auction.NewBiddingWindow(t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auction.New(id, seller, src.Lot(), src.StartPrice(), src.Increment(),
		auction.NoReserve(), window, rules, t0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// assertSameAuction compares two aggregates getter-by-getter; times
// via Equal so storage precision/zone differences do not matter.
func assertSameAuction(t *testing.T, want, got *auction.Auction) {
	t.Helper()
	if got.ID() != want.ID() || got.Seller() != want.Seller() || got.Lot() != want.Lot() {
		t.Fatal("identity/lot mismatch")
	}
	if got.StartPrice() != want.StartPrice() || got.Increment() != want.Increment() {
		t.Fatal("price mismatch")
	}
	if got.Reserve() != want.Reserve() {
		t.Fatal("reserve mismatch")
	}
	if !got.Window().StartsAt().Equal(want.Window().StartsAt()) ||
		!got.Window().EndsAt().Equal(want.Window().EndsAt()) ||
		!got.Window().OriginalEndsAt().Equal(want.Window().OriginalEndsAt()) {
		t.Fatal("window mismatch")
	}
	if got.AntiSnipe() != want.AntiSnipe() || got.VerifyAbove() != want.VerifyAbove() {
		t.Fatal("rule snapshot mismatch")
	}
	if got.Status() != want.Status() || got.Outcome() != want.Outcome() {
		t.Fatal("status mismatch")
	}
	assertSameBid(t, "leading", want.LeadingBid(), got.LeadingBid())
	assertSameBid(t, "runner-up", want.RunnerUpBid(), got.RunnerUpBid())
	if got.WinnerReassigned() != want.WinnerReassigned() || got.BidCount() != want.BidCount() ||
		got.RelistOf() != want.RelistOf() || got.RelistGeneration() != want.RelistGeneration() ||
		got.Settled() != want.Settled() || got.ExtensionsUsed() != want.ExtensionsUsed() {
		t.Fatal("denormalized state mismatch")
	}
}

func assertSameBid(t *testing.T, label string, want, got auction.Bid) {
	t.Helper()
	if want.IsZero() != got.IsZero() {
		t.Fatalf("%s bid presence mismatch", label)
	}
	if want.IsZero() {
		return
	}
	if got.ID() != want.ID() || got.Bidder() != want.Bidder() || got.Amount() != want.Amount() {
		t.Fatalf("%s bid mismatch", label)
	}
	if !got.PlacedAt().Equal(want.PlacedAt()) {
		t.Fatalf("%s bid placedAt mismatch", label)
	}
}
