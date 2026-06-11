package auction_test

import (
	"errors"
	"testing"
	"time"

	"molot/internal/auction/domain/auction"
)

func TestAuction_Close(t *testing.T) {
	t.Parallel()

	afterEnd := t0.Add(25 * time.Hour)

	t.Run("no bids closes as not sold", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != auction.OutcomeNotSold {
			t.Fatalf("outcome = %v, want not sold", res.Outcome)
		}
		if !res.Winner.IsZero() {
			t.Fatal("winner must be zero when not sold")
		}
		if a.Status() != auction.StatusClosed {
			t.Fatalf("status = %v, want closed", a.Status())
		}
	})

	t.Run("bids without reserve close as sold", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		winner := placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != auction.OutcomeSold {
			t.Fatalf("outcome = %v, want sold", res.Outcome)
		}
		if res.Winner != winner {
			t.Fatal("winner must be the leading bid")
		}
	})

	t.Run("reserve met closes as sold", func(t *testing.T) {
		t.Parallel()
		a := listedAuctionWithReserve(t, 5000)
		placeBid(t, a, verifiedBidder(t), eur(t, 5000), t0.Add(time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != auction.OutcomeSold {
			t.Fatalf("outcome = %v, want sold (reserve met exactly)", res.Outcome)
		}
	})

	t.Run("reserve unmet closes as not sold", func(t *testing.T) {
		t.Parallel()
		a := listedAuctionWithReserve(t, 5000)
		placeBid(t, a, verifiedBidder(t), eur(t, 4999), t0.Add(time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != auction.OutcomeNotSold {
			t.Fatalf("outcome = %v, want not sold (reserve unmet)", res.Outcome)
		}
		if !res.Winner.IsZero() {
			t.Fatal("winner must be zero when reserve unmet")
		}
	})

	t.Run("before deadline fails with ErrBiddingStillOpen", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if _, err := a.Close(t0.Add(23 * time.Hour)); !errors.Is(err, auction.ErrBiddingStillOpen) {
			t.Fatalf("err = %v, want ErrBiddingStillOpen", err)
		}
		if a.Status() != auction.StatusListed {
			t.Fatal("failed close must not change status")
		}
	})

	t.Run("exactly at deadline closes", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if _, err := a.Close(t0.Add(24 * time.Hour)); err != nil {
			t.Fatalf("close at deadline: %v", err)
		}
	})

	t.Run("cancelled auction fails with ErrAuctionCancelled", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if err := a.Cancel(a.Seller(), t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Close(afterEnd); !errors.Is(err, auction.ErrAuctionCancelled) {
			t.Fatalf("err = %v, want ErrAuctionCancelled", err)
		}
	})

	t.Run("second close fails with ErrAlreadyClosed", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if _, err := a.Close(afterEnd); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Close(afterEnd.Add(time.Hour)); !errors.Is(err, auction.ErrAlreadyClosed) {
			t.Fatalf("err = %v, want ErrAlreadyClosed", err)
		}
	})

	t.Run("runner-up qualifies when reserve met", func(t *testing.T) {
		t.Parallel()
		a := listedAuctionWithReserve(t, 1000)
		runnerUp := placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		placeBid(t, a, verifiedBidder(t), eur(t, 1100), t0.Add(2*time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if !res.RunnerUpQualifies {
			t.Fatal("runner-up at reserve must qualify")
		}
		if res.RunnerUp != runnerUp {
			t.Fatal("result must carry the runner-up bid")
		}
	})

	t.Run("runner-up below reserve does not qualify", func(t *testing.T) {
		t.Parallel()
		a := listedAuctionWithReserve(t, 1100)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		placeBid(t, a, verifiedBidder(t), eur(t, 1100), t0.Add(2*time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != auction.OutcomeSold {
			t.Fatal("leading bid meets reserve, must be sold")
		}
		if res.RunnerUpQualifies {
			t.Fatal("runner-up below reserve must not qualify")
		}
	})

	t.Run("no runner-up does not qualify", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if res.RunnerUpQualifies {
			t.Fatal("absent runner-up must not qualify")
		}
	})

	t.Run("no reserve: runner-up qualifies", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		placeBid(t, a, verifiedBidder(t), eur(t, 1100), t0.Add(2*time.Hour))
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		if !res.RunnerUpQualifies {
			t.Fatal("runner-up with no reserve must qualify")
		}
	})

	t.Run("records AuctionClosed with the result", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		a.PullDomainEvents()
		res, err := a.Close(afterEnd)
		if err != nil {
			t.Fatal(err)
		}
		events := a.PullDomainEvents()
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		closed, ok := events[0].(auction.AuctionClosed)
		if !ok {
			t.Fatalf("event = %T, want AuctionClosed", events[0])
		}
		if closed.Result != res {
			t.Fatal("event must carry the closing result")
		}
		if !closed.OccurredAt.Equal(afterEnd) {
			t.Fatalf("occurredAt = %v, want %v", closed.OccurredAt, afterEnd)
		}
	})
}

func TestAuction_Cancel(t *testing.T) {
	t.Parallel()

	t.Run("seller cancels a bidless listing", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if err := a.Cancel(a.Seller(), t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if a.Status() != auction.StatusCancelled {
			t.Fatalf("status = %v, want cancelled", a.Status())
		}
		events := a.PullDomainEvents()
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		if _, ok := events[0].(auction.AuctionCancelled); !ok {
			t.Fatalf("event = %T, want AuctionCancelled", events[0])
		}
	})

	t.Run("non-seller is forbidden", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		stranger := newSellerID(t)
		err := a.Cancel(stranger, t0.Add(time.Hour))
		var forbidden auction.ForbiddenAuctionManagementError
		if !errors.As(err, &forbidden) {
			t.Fatalf("err = %v, want ForbiddenAuctionManagementError", err)
		}
		if forbidden.Actor != stranger || forbidden.Owner != a.Seller() {
			t.Fatal("forbidden error must carry both principals")
		}
	})

	t.Run("with bids fails with ErrAuctionHasBids", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		if err := a.Cancel(a.Seller(), t0.Add(2*time.Hour)); !errors.Is(err, auction.ErrAuctionHasBids) {
			t.Fatalf("err = %v, want ErrAuctionHasBids", err)
		}
	})

	t.Run("closed auction fails with ErrAlreadyClosed", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := a.Cancel(a.Seller(), t0.Add(26*time.Hour)); !errors.Is(err, auction.ErrAlreadyClosed) {
			t.Fatalf("err = %v, want ErrAlreadyClosed", err)
		}
	})

	t.Run("second cancel fails with ErrAuctionCancelled", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if err := a.Cancel(a.Seller(), t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := a.Cancel(a.Seller(), t0.Add(2*time.Hour)); !errors.Is(err, auction.ErrAuctionCancelled) {
			t.Fatalf("err = %v, want ErrAuctionCancelled", err)
		}
	})
}
