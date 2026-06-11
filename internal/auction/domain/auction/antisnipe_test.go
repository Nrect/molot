package auction_test

import (
	"errors"
	"testing"
	"time"

	"molot/internal/auction/domain/auction"
)

// Anti-snipe fixture: window 5m, extension 10m, max 2 (testRules).
// The auction ends at t0+24h.

func TestAuction_AntiSnipe(t *testing.T) {
	t.Parallel()

	end := t0.Add(24 * time.Hour)

	t.Run("bid inside snipe window extends the deadline", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-3*time.Minute))
		if got, want := a.EndsAt(), end.Add(10*time.Minute); !got.Equal(want) {
			t.Fatalf("endsAt = %v, want %v", got, want)
		}
		if a.ExtensionsUsed() != 1 {
			t.Fatalf("extensionsUsed = %d, want 1", a.ExtensionsUsed())
		}
	})

	t.Run("bid outside snipe window does not extend", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-6*time.Minute))
		if !a.EndsAt().Equal(end) {
			t.Fatalf("endsAt = %v, want unchanged %v", a.EndsAt(), end)
		}
		if a.ExtensionsUsed() != 0 {
			t.Fatalf("extensionsUsed = %d, want 0", a.ExtensionsUsed())
		}
	})

	t.Run("boundary: bid exactly at endsAt-window triggers", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-5*time.Minute))
		if !a.EndsAt().Equal(end.Add(10 * time.Minute)) {
			t.Fatal("bid exactly at the window edge must extend")
		}
	})

	t.Run("extensions stop at maxExtensions", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		bidders := []auction.Bidder{verifiedBidder(t), verifiedBidder(t), verifiedBidder(t)}

		// Extension 1: ends 24h -> 24h10m.
		placeBid(t, a, bidders[0], eur(t, 1000), end.Add(-time.Minute))
		// Extension 2: inside the new window; ends -> 24h20m.
		placeBid(t, a, bidders[1], eur(t, 1100), end.Add(9*time.Minute))
		if a.ExtensionsUsed() != 2 {
			t.Fatalf("extensionsUsed = %d, want 2", a.ExtensionsUsed())
		}
		// Third snipe bid: cap reached, no further extension.
		placeBid(t, a, bidders[2], eur(t, 1200), end.Add(19*time.Minute))
		if got, want := a.EndsAt(), end.Add(20*time.Minute); !got.Equal(want) {
			t.Fatalf("endsAt = %v, want capped %v", got, want)
		}
		if a.ExtensionsUsed() != 2 {
			t.Fatalf("extensionsUsed = %d, want still 2", a.ExtensionsUsed())
		}
	})

	t.Run("originalEndsAt survives extensions", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-time.Minute))
		if !a.Window().OriginalEndsAt().Equal(end) {
			t.Fatalf("originalEndsAt = %v, want %v", a.Window().OriginalEndsAt(), end)
		}
	})

	t.Run("extension records AuctionExtended and flags BidPlaced", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		bid := placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-time.Minute))

		events := a.PullDomainEvents()
		if len(events) != 2 {
			t.Fatalf("events = %d, want AuctionExtended + BidPlaced", len(events))
		}
		extendedEvent, ok := events[0].(auction.AuctionExtended)
		if !ok {
			t.Fatalf("first event = %T, want AuctionExtended", events[0])
		}
		if !extendedEvent.NewEndsAt.Equal(end.Add(10 * time.Minute)) {
			t.Fatalf("AuctionExtended.NewEndsAt = %v", extendedEvent.NewEndsAt)
		}
		placed, ok := events[1].(auction.BidPlaced)
		if !ok {
			t.Fatalf("second event = %T, want BidPlaced", events[1])
		}
		if !placed.Extended {
			t.Fatal("BidPlaced.Extended must be true")
		}
		if !placed.NewEndsAt.Equal(end.Add(10 * time.Minute)) {
			t.Fatalf("BidPlaced.NewEndsAt = %v", placed.NewEndsAt)
		}
		if placed.Bid != bid {
			t.Fatal("BidPlaced must carry the accepted bid")
		}
	})

	t.Run("non-snipe bid keeps Extended false", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		events := a.PullDomainEvents()
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		if events[0].(auction.BidPlaced).Extended {
			t.Fatal("BidPlaced.Extended must be false outside the snipe window")
		}
	})

	t.Run("close at old deadline fails after extension", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-time.Minute))
		// The worker scanned before the extension: closing at the old
		// deadline must be refused (§10 race).
		if _, err := a.Close(end); !errors.Is(err, auction.ErrBiddingStillOpen) {
			t.Fatalf("err = %v, want ErrBiddingStillOpen", err)
		}
		if _, err := a.Close(end.Add(10 * time.Minute)); err != nil {
			t.Fatalf("close at the extended deadline: %v", err)
		}
	})

	t.Run("bid during extension period is accepted", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-time.Minute))
		// Past the original deadline but inside the extension.
		placeBid(t, a, verifiedBidder(t), eur(t, 1100), end.Add(5*time.Minute))
		if a.BidCount() != 2 {
			t.Fatalf("bidCount = %d, want 2", a.BidCount())
		}
	})
}
