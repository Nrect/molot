package auction_test

import (
	"errors"
	"testing"
	"time"

	"molot/internal/auction/domain/auction"
)

var sagaTime = t0.Add(30 * time.Hour)

func TestAuction_AwardToRunnerUp(t *testing.T) {
	t.Parallel()

	t.Run("promotes runner-up to winner exactly once", func(t *testing.T) {
		t.Parallel()
		a, _, runnerUp := closedSoldAuction(t)

		newWinner, err := a.AwardToRunnerUp(sagaTime)
		if err != nil {
			t.Fatal(err)
		}
		if newWinner != runnerUp {
			t.Fatal("new winner must be the runner-up bid")
		}
		if a.LeadingBid() != runnerUp || !a.RunnerUpBid().IsZero() {
			t.Fatal("leading must become runner-up, runner-up must clear")
		}
		if !a.WinnerReassigned() {
			t.Fatal("winnerReassigned marker must be set")
		}
		events := a.PullDomainEvents()
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		reassigned, ok := events[0].(auction.WinnerReassigned)
		if !ok {
			t.Fatalf("event = %T, want WinnerReassigned", events[0])
		}
		if reassigned.NewWinner != runnerUp {
			t.Fatal("event must carry the new winner")
		}

		// Repeat: sentinel, no second event (facade idempotency, §2.1).
		if _, err := a.AwardToRunnerUp(sagaTime.Add(time.Hour)); !errors.Is(err, auction.ErrWinnerAlreadyReassigned) {
			t.Fatalf("repeat err = %v, want ErrWinnerAlreadyReassigned", err)
		}
		if len(a.PullDomainEvents()) != 0 {
			t.Fatal("repeat must not record events")
		}
	})

	t.Run("without runner-up fails with ErrNoQualifyingRunnerUp", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		placeBid(t, a, verifiedBidder(t), eur(t, 1000), t0.Add(time.Hour))
		if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.AwardToRunnerUp(sagaTime); !errors.Is(err, auction.ErrNoQualifyingRunnerUp) {
			t.Fatalf("err = %v, want ErrNoQualifyingRunnerUp", err)
		}
	})

	t.Run("on a still-open auction fails", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if _, err := a.AwardToRunnerUp(sagaTime); !errors.Is(err, auction.ErrBiddingStillOpen) {
			t.Fatalf("err = %v, want ErrBiddingStillOpen", err)
		}
	})

	t.Run("on a settled sale fails with ErrAlreadySettled", func(t *testing.T) {
		t.Parallel()
		a, _, _ := closedSoldAuction(t)
		if err := a.ConfirmSettlement(sagaTime); err != nil {
			t.Fatal(err)
		}
		if _, err := a.AwardToRunnerUp(sagaTime); !errors.Is(err, auction.ErrAlreadySettled) {
			t.Fatalf("err = %v, want ErrAlreadySettled", err)
		}
	})

	t.Run("on a not-sold close fails with ErrNoQualifyingRunnerUp", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if _, err := a.Close(t0.Add(25 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.AwardToRunnerUp(sagaTime); !errors.Is(err, auction.ErrNoQualifyingRunnerUp) {
			t.Fatalf("err = %v, want ErrNoQualifyingRunnerUp", err)
		}
	})
}

func TestAuction_ConfirmSettlement(t *testing.T) {
	t.Parallel()

	t.Run("settles a sold auction", func(t *testing.T) {
		t.Parallel()
		a, _, _ := closedSoldAuction(t)
		if err := a.ConfirmSettlement(sagaTime); err != nil {
			t.Fatal(err)
		}
		if !a.Settled() {
			t.Fatal("settled must be true")
		}
		events := a.PullDomainEvents()
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		if _, ok := events[0].(auction.SaleSettled); !ok {
			t.Fatalf("event = %T, want SaleSettled", events[0])
		}

		// Repeat: sentinel, no event.
		if err := a.ConfirmSettlement(sagaTime); !errors.Is(err, auction.ErrAlreadySettled) {
			t.Fatalf("repeat err = %v, want ErrAlreadySettled", err)
		}
		if len(a.PullDomainEvents()) != 0 {
			t.Fatal("repeat must not record events")
		}
	})

	t.Run("after FailSale fails with ErrSaleAlreadyFailed", func(t *testing.T) {
		t.Parallel()
		a, _, _ := closedSoldAuction(t)
		if err := a.FailSale(auction.ReasonPaymentTimeout, sagaTime); err != nil {
			t.Fatal(err)
		}
		if err := a.ConfirmSettlement(sagaTime); !errors.Is(err, auction.ErrSaleAlreadyFailed) {
			t.Fatalf("err = %v, want ErrSaleAlreadyFailed", err)
		}
	})

	t.Run("on an open auction fails", func(t *testing.T) {
		t.Parallel()
		a := listedAuction(t)
		if err := a.ConfirmSettlement(sagaTime); !errors.Is(err, auction.ErrBiddingStillOpen) {
			t.Fatalf("err = %v, want ErrBiddingStillOpen", err)
		}
	})
}

func TestAuction_FailSale(t *testing.T) {
	t.Parallel()

	t.Run("fails a sold auction with a reason", func(t *testing.T) {
		t.Parallel()
		a, _, _ := closedSoldAuction(t)
		if err := a.FailSale(auction.ReasonSecondChanceDeclined, sagaTime); err != nil {
			t.Fatal(err)
		}
		if a.Outcome() != auction.OutcomeNotSold {
			t.Fatalf("outcome = %v, want not sold", a.Outcome())
		}
		if !a.Settled() {
			t.Fatal("settled must be true after failure")
		}
		events := a.PullDomainEvents()
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		failed, ok := events[0].(auction.SaleFailed)
		if !ok {
			t.Fatalf("event = %T, want SaleFailed", events[0])
		}
		if failed.Reason != auction.ReasonSecondChanceDeclined {
			t.Fatalf("reason = %v", failed.Reason)
		}

		// Repeat: sentinel, no event.
		if err := a.FailSale(auction.ReasonPaymentTimeout, sagaTime); !errors.Is(err, auction.ErrSaleAlreadyFailed) {
			t.Fatalf("repeat err = %v, want ErrSaleAlreadyFailed", err)
		}
		if len(a.PullDomainEvents()) != 0 {
			t.Fatal("repeat must not record events")
		}
	})

	t.Run("a settled-success sale cannot fail", func(t *testing.T) {
		t.Parallel()
		a, _, _ := closedSoldAuction(t)
		if err := a.ConfirmSettlement(sagaTime); err != nil {
			t.Fatal(err)
		}
		if err := a.FailSale(auction.ReasonPaymentTimeout, sagaTime); !errors.Is(err, auction.ErrAlreadySettled) {
			t.Fatalf("err = %v, want ErrAlreadySettled", err)
		}
	})

	t.Run("zero reason is rejected", func(t *testing.T) {
		t.Parallel()
		a, _, _ := closedSoldAuction(t)
		if err := a.FailSale(auction.FailureReason{}, sagaTime); !errors.Is(err, auction.ErrInvalidFailureReason) {
			t.Fatalf("err = %v, want ErrInvalidFailureReason", err)
		}
	})
}

func TestRelistedFrom(t *testing.T) {
	t.Parallel()

	relistWindow := func(t *testing.T) auction.BiddingWindow {
		w, err := auction.NewBiddingWindow(sagaTime.Add(time.Hour), sagaTime.Add(25*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	t.Run("inherits lot, prices and rule snapshot", func(t *testing.T) {
		t.Parallel()
		orig := listedAuctionWithReserve(t, 5000)
		newID := newAuctionID(t)
		w := relistWindow(t)

		relisted, err := auction.RelistedFrom(orig, newID, w, sagaTime)
		if err != nil {
			t.Fatal(err)
		}
		if relisted.ID() != newID {
			t.Fatal("relisted id mismatch")
		}
		if relisted.Seller() != orig.Seller() || relisted.Lot() != orig.Lot() {
			t.Fatal("seller/lot must be inherited")
		}
		if relisted.StartPrice() != orig.StartPrice() || relisted.Increment() != orig.Increment() {
			t.Fatal("prices must be inherited")
		}
		if relisted.Reserve() != orig.Reserve() {
			t.Fatal("reserve must be inherited")
		}
		if relisted.AntiSnipe() != orig.AntiSnipe() || relisted.VerifyAbove() != orig.VerifyAbove() {
			t.Fatal("rule snapshot must be inherited")
		}
		if relisted.RelistOf() != orig.ID() {
			t.Fatal("relistOf must reference the original")
		}
		if relisted.RelistGeneration() != 1 {
			t.Fatalf("relistGen = %d, want 1", relisted.RelistGeneration())
		}
		if relisted.Status() != auction.StatusListed {
			t.Fatal("relisted auction must be listed")
		}
		if relisted.HasBids() {
			t.Fatal("relisted auction must start without bids")
		}
	})

	t.Run("records AuctionListed and AuctionRelisted", func(t *testing.T) {
		t.Parallel()
		orig := listedAuction(t)
		newID := newAuctionID(t)
		w := relistWindow(t)
		relisted, err := auction.RelistedFrom(orig, newID, w, sagaTime)
		if err != nil {
			t.Fatal(err)
		}
		events := relisted.PullDomainEvents()
		if len(events) != 2 {
			t.Fatalf("events = %d, want 2", len(events))
		}
		if _, ok := events[0].(auction.AuctionListed); !ok {
			t.Fatalf("first event = %T, want AuctionListed", events[0])
		}
		relistedEvent, ok := events[1].(auction.AuctionRelisted)
		if !ok {
			t.Fatalf("second event = %T, want AuctionRelisted", events[1])
		}
		if relistedEvent.OriginalID != orig.ID() || relistedEvent.NewID != newID {
			t.Fatal("AuctionRelisted must reference both auctions")
		}
	})

	t.Run("generation cap: relist of a relist fails", func(t *testing.T) {
		t.Parallel()
		orig := listedAuction(t)
		gen1, err := auction.RelistedFrom(orig, newAuctionID(t), relistWindow(t), sagaTime)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := auction.RelistedFrom(gen1, newAuctionID(t), relistWindow(t), sagaTime); !errors.Is(err, auction.ErrRelistLimitReached) {
			t.Fatalf("err = %v, want ErrRelistLimitReached", err)
		}
	})

	t.Run("zero new id is rejected", func(t *testing.T) {
		t.Parallel()
		orig := listedAuction(t)
		if _, err := auction.RelistedFrom(orig, auction.AuctionID{}, relistWindow(t), sagaTime); !errors.Is(err, auction.ErrInvalidID) {
			t.Fatalf("err = %v, want ErrInvalidID", err)
		}
	})

	t.Run("window ending in the past is rejected", func(t *testing.T) {
		t.Parallel()
		orig := listedAuction(t)
		w, err := auction.NewBiddingWindow(t0, t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := auction.RelistedFrom(orig, newAuctionID(t), w, sagaTime); !errors.Is(err, auction.ErrInvalidBiddingWindow) {
			t.Fatalf("err = %v, want ErrInvalidBiddingWindow", err)
		}
	})
}
