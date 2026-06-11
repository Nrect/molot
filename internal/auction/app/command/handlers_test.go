package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"molot/internal/auction/app/command"
	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

func TestListAuctionHandler(t *testing.T) {
	t.Parallel()

	t.Run("adds a listed auction", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		h := command.NewListAuctionHandler(repo, testRules(t), fixedClock{now0})
		a := listedAuction(t)

		err := h.Handle(context.Background(), command.ListAuction{
			AuctionID:  a.ID(),
			Seller:     a.Seller(),
			Lot:        a.Lot(),
			StartPrice: a.StartPrice(),
			Increment:  a.Increment(),
			Reserve:    auction.NoReserve(),
			Window:     testWindow(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(repo.added) != 1 || repo.added[0] != a.ID() {
			t.Fatalf("added = %v, want exactly the new auction", repo.added)
		}
	})

	t.Run("maps invalid window to a 400 slug", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		h := command.NewListAuctionHandler(repo, testRules(t), fixedClock{now0})
		a := listedAuction(t)

		err := h.Handle(context.Background(), command.ListAuction{
			AuctionID:  a.ID(),
			Seller:     a.Seller(),
			Lot:        a.Lot(),
			StartPrice: a.StartPrice(),
			Increment:  a.Increment(),
			Reserve:    auction.NoReserve(),
			Window:     auction.BiddingWindow{},
		})
		if !errors.Is(err, errs.NewIncorrectInputError("invalid-window")) {
			t.Fatalf("err = %v, want slug invalid-window", err)
		}
		if !errors.Is(err, auction.ErrInvalidBiddingWindow) {
			t.Fatal("slug error must keep the domain cause")
		}
		if len(repo.added) != 0 {
			t.Fatal("nothing must be added on validation failure")
		}
	})
}

func TestPlaceBidHandler(t *testing.T) {
	t.Parallel()

	t.Run("updates with the bidder as actor", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := listedAuction(t)
		repo.seed(a)
		profiles := newSpyProfiles()
		bidderID := newBidderID(t)
		h := command.NewPlaceBidHandler(repo, profiles, fixedClock{now0.Add(time.Hour)})

		err := h.Handle(context.Background(), command.PlaceBid{
			AuctionID: a.ID(), BidID: newBidID(t), Bidder: bidderID, Amount: eur(t, 1000),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(profiles.asked) != 1 || profiles.asked[0] != bidderID {
			t.Fatalf("profiles asked = %v, want the bidder", profiles.asked)
		}
		if len(repo.updates) != 1 {
			t.Fatalf("updates = %d, want 1", len(repo.updates))
		}
		call := repo.updates[0]
		if call.id != a.ID() || call.system || call.actor != auction.ActorFromBidder(bidderID) {
			t.Fatalf("update call = %+v, want user update by the bidder", call)
		}
		got, _ := repo.Get(context.Background(), a.ID())
		if got.BidCount() != 1 {
			t.Fatal("bid must be persisted through updateFn")
		}
	})

	t.Run("maps below-minimum to a conflict slug", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := listedAuction(t)
		repo.seed(a)
		h := command.NewPlaceBidHandler(repo, newSpyProfiles(), fixedClock{now0.Add(time.Hour)})

		err := h.Handle(context.Background(), command.PlaceBid{
			AuctionID: a.ID(), BidID: newBidID(t), Bidder: newBidderID(t), Amount: eur(t, 1),
		})
		if !errors.Is(err, errs.NewConflictError("bid-below-minimum")) {
			t.Fatalf("err = %v, want slug bid-below-minimum", err)
		}
	})

	t.Run("unknown bidder is unverified: big bid needs verification", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := listedAuction(t)
		repo.seed(a)
		h := command.NewPlaceBidHandler(repo, newSpyProfiles(), fixedClock{now0.Add(time.Hour)})

		err := h.Handle(context.Background(), command.PlaceBid{
			AuctionID: a.ID(), BidID: newBidID(t), Bidder: newBidderID(t), Amount: eur(t, 200_000),
		})
		if !errors.Is(err, errs.NewForbiddenError("verification-required")) {
			t.Fatalf("err = %v, want slug verification-required", err)
		}
	})

	t.Run("missing auction maps to not-found slug", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		h := command.NewPlaceBidHandler(repo, newSpyProfiles(), fixedClock{now0})

		err := h.Handle(context.Background(), command.PlaceBid{
			AuctionID: newAuctionID(t), BidID: newBidID(t), Bidder: newBidderID(t), Amount: eur(t, 1000),
		})
		if !errors.Is(err, errs.NewNotFoundError("auction-not-found")) {
			t.Fatalf("err = %v, want slug auction-not-found", err)
		}
	})
}

func TestCancelAuctionHandler_MapsForbidden(t *testing.T) {
	t.Parallel()
	repo := newSpyRepo()
	a := listedAuction(t)
	repo.seed(a)
	h := command.NewCancelAuctionHandler(repo, fixedClock{now0.Add(time.Hour)})

	strangerUUID := newAuctionID(t).UUID() // any foreign uuid
	stranger, err := auction.NewSellerID(strangerUUID)
	if err != nil {
		t.Fatal(err)
	}
	got := h.Handle(context.Background(), command.CancelAuction{AuctionID: a.ID(), Seller: stranger})
	if !errors.Is(got, errs.NewForbiddenError("not-seller")) {
		t.Fatalf("err = %v, want slug not-seller", got)
	}
}

func TestCloseAuctionHandler_BenignOutcomesAreNoOps(t *testing.T) {
	t.Parallel()

	t.Run("already closed maps to nil", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		h := command.NewCloseAuctionHandler(repo, fixedClock{now0.Add(26 * time.Hour)})
		if err := h.Handle(context.Background(), command.CloseAuction{AuctionID: a.ID()}); err != nil {
			t.Fatalf("err = %v, want nil (idempotent close)", err)
		}
		if !repo.updates[0].system {
			t.Fatal("close must use UpdateAsSystem")
		}
	})

	t.Run("still open maps to nil (extension race)", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := listedAuction(t)
		repo.seed(a)
		h := command.NewCloseAuctionHandler(repo, fixedClock{now0.Add(time.Hour)})
		if err := h.Handle(context.Background(), command.CloseAuction{AuctionID: a.ID()}); err != nil {
			t.Fatalf("err = %v, want nil (still open is a skip)", err)
		}
		got, _ := repo.Get(context.Background(), a.ID())
		if got.Status() != auction.StatusListed {
			t.Fatal("auction must stay listed")
		}
	})

	t.Run("closes a due auction", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := listedAuction(t)
		repo.seed(a)
		h := command.NewCloseAuctionHandler(repo, fixedClock{now0.Add(25 * time.Hour)})
		if err := h.Handle(context.Background(), command.CloseAuction{AuctionID: a.ID()}); err != nil {
			t.Fatal(err)
		}
		got, _ := repo.Get(context.Background(), a.ID())
		if got.Status() != auction.StatusClosed {
			t.Fatal("auction must be closed")
		}
	})
}

func TestSagaFacadeHandlers_AlreadyDoneIsNil(t *testing.T) {
	t.Parallel()

	t.Run("AwardToRunnerUp repeat is nil", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		h := command.NewAwardToRunnerUpHandler(repo, fixedClock{now0.Add(30 * time.Hour)})
		cmd := command.AwardToRunnerUp{AuctionID: a.ID()}

		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatalf("repeat err = %v, want nil", err)
		}
		got, _ := repo.Get(context.Background(), a.ID())
		if !got.WinnerReassigned() {
			t.Fatal("winner must be reassigned")
		}
	})

	t.Run("ConfirmSettlement repeat is nil", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		h := command.NewConfirmSettlementHandler(repo, fixedClock{now0.Add(30 * time.Hour)})
		cmd := command.ConfirmSettlement{AuctionID: a.ID()}

		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatalf("repeat err = %v, want nil", err)
		}
	})

	t.Run("MarkSaleFailed repeat is nil", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		h := command.NewMarkSaleFailedHandler(repo, fixedClock{now0.Add(30 * time.Hour)})
		cmd := command.MarkSaleFailed{AuctionID: a.ID(), Reason: auction.ReasonPaymentTimeout}

		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatalf("repeat err = %v, want nil", err)
		}
		got, _ := repo.Get(context.Background(), a.ID())
		if got.Outcome() != auction.OutcomeNotSold {
			t.Fatal("outcome must be not_sold")
		}
	})

	t.Run("MarkSaleFailed on settled sale is a conflict", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		confirm := command.NewConfirmSettlementHandler(repo, fixedClock{now0.Add(30 * time.Hour)})
		if err := confirm.Handle(context.Background(), command.ConfirmSettlement{AuctionID: a.ID()}); err != nil {
			t.Fatal(err)
		}
		fail := command.NewMarkSaleFailedHandler(repo, fixedClock{now0.Add(31 * time.Hour)})
		err := fail.Handle(context.Background(), command.MarkSaleFailed{AuctionID: a.ID(), Reason: auction.ReasonPaymentTimeout})
		if !errors.Is(err, errs.NewConflictError("already-settled")) {
			t.Fatalf("err = %v, want slug already-settled", err)
		}
	})

	t.Run("RelistAuction repeat is nil", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		h := command.NewRelistAuctionHandler(repo, fixedClock{now0.Add(30 * time.Hour)})
		w, err := auction.NewBiddingWindow(now0.Add(31*time.Hour), now0.Add(55*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		cmd := command.RelistAuction{OriginalID: a.ID(), NewID: newAuctionID(t), Window: w}

		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		// The spy Add overwrites silently, mirroring ON CONFLICT DO
		// NOTHING for the same id: the handler must stay nil.
		if err := h.Handle(context.Background(), cmd); err != nil {
			t.Fatalf("repeat err = %v, want nil", err)
		}
		if len(repo.added) != 2 {
			t.Fatalf("added calls = %d, want 2 (idempotency is the store's job)", len(repo.added))
		}
	})

	t.Run("RelistAuction maps ErrAlreadyRelisted from Add to nil", func(t *testing.T) {
		t.Parallel()
		repo := newSpyRepo()
		a := closedSoldAuction(t, repo)
		repo.addErr = auction.ErrAlreadyRelisted
		h := command.NewRelistAuctionHandler(repo, fixedClock{now0.Add(30 * time.Hour)})
		w, err := auction.NewBiddingWindow(now0.Add(31*time.Hour), now0.Add(55*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Handle(context.Background(), command.RelistAuction{
			OriginalID: a.ID(), NewID: newAuctionID(t), Window: w,
		}); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
}
