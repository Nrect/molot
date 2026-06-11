package auction_test

import (
	"errors"
	"testing"
	"time"

	"molot/internal/auction/domain/auction"
)

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	type args struct {
		id         auction.AuctionID
		seller     auction.SellerID
		lot        auction.Lot
		startPrice auction.Money
		increment  auction.Money
		reserve    auction.ReservePrice
		window     auction.BiddingWindow
		rules      auction.ListingRules
		now        time.Time
	}

	valid := func(t *testing.T) args {
		return args{
			id:         newAuctionID(t),
			seller:     newSellerID(t),
			lot:        testLot(t),
			startPrice: eur(t, 1000),
			increment:  eur(t, 100),
			reserve:    auction.NoReserve(),
			window:     testWindow(t),
			rules:      testRules(t),
			now:        t0,
		}
	}

	tests := []struct {
		name    string
		mutate  func(t *testing.T, a *args)
		wantErr error
	}{
		{"valid listing", func(t *testing.T, a *args) {}, nil},
		{"zero id", func(t *testing.T, a *args) { a.id = auction.AuctionID{} }, auction.ErrInvalidID},
		{"zero seller", func(t *testing.T, a *args) { a.seller = auction.SellerID{} }, auction.ErrInvalidID},
		{"zero lot", func(t *testing.T, a *args) { a.lot = auction.Lot{} }, auction.ErrInvalidLot},
		{"zero start price", func(t *testing.T, a *args) { a.startPrice = auction.Money{} }, auction.ErrInvalidStartPrice},
		{"non-platform currency", func(t *testing.T, a *args) {
			a.startPrice = usd(t, 1000)
			a.increment = usd(t, 100)
		}, auction.ErrUnsupportedCurrency},
		{"zero increment", func(t *testing.T, a *args) { a.increment = auction.Money{} }, auction.ErrInvalidIncrement},
		{"increment currency mismatch", func(t *testing.T, a *args) { a.increment = usd(t, 100) }, auction.ErrCurrencyMismatch},
		{"reserve currency mismatch", func(t *testing.T, a *args) {
			r, err := auction.NewReservePrice(usd(t, 5000))
			if err != nil {
				t.Fatal(err)
			}
			a.reserve = r
		}, auction.ErrCurrencyMismatch},
		{"zero window", func(t *testing.T, a *args) { a.window = auction.BiddingWindow{} }, auction.ErrInvalidBiddingWindow},
		{"window already over", func(t *testing.T, a *args) { a.now = t0.Add(48 * time.Hour) }, auction.ErrInvalidBiddingWindow},
		{"zero rules", func(t *testing.T, a *args) { a.rules = auction.ListingRules{} }, auction.ErrInvalidListingRules},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := valid(t)
			tt.mutate(t, &in)

			a, err := auction.New(in.id, in.seller, in.lot, in.startPrice, in.increment,
				in.reserve, in.window, in.rules, in.now)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.Status() != auction.StatusListed {
				t.Fatalf("status = %v, want listed", a.Status())
			}
			if a.Version() != 1 {
				t.Fatalf("version = %d, want 1", a.Version())
			}
			events := a.PullDomainEvents()
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if _, ok := events[0].(auction.AuctionListed); !ok {
				t.Fatalf("event = %T, want AuctionListed", events[0])
			}
		})
	}
}

func TestValueObjects(t *testing.T) {
	t.Parallel()

	t.Run("currency must be 3 uppercase letters", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"", "EU", "EURO", "eur", "E1R"} {
			if _, err := auction.NewCurrency(bad); !errors.Is(err, auction.ErrInvalidCurrency) {
				t.Fatalf("NewCurrency(%q) err = %v, want ErrInvalidCurrency", bad, err)
			}
		}
		if _, err := auction.NewCurrency("EUR"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("money rejects negative amounts", func(t *testing.T) {
		t.Parallel()
		c, _ := auction.NewCurrency("EUR")
		if _, err := auction.NewMoney(-1, c); !errors.Is(err, auction.ErrNegativeAmount) {
			t.Fatalf("err = %v, want ErrNegativeAmount", err)
		}
	})

	t.Run("money Add and GTE reject currency mismatch", func(t *testing.T) {
		t.Parallel()
		if _, err := eur(t, 1).Add(usd(t, 1)); !errors.Is(err, auction.ErrCurrencyMismatch) {
			t.Fatalf("Add err = %v, want ErrCurrencyMismatch", err)
		}
		if _, err := eur(t, 1).GTE(usd(t, 1)); !errors.Is(err, auction.ErrCurrencyMismatch) {
			t.Fatalf("GTE err = %v, want ErrCurrencyMismatch", err)
		}
		sum, err := eur(t, 1000).Add(eur(t, 100))
		if err != nil || sum != eur(t, 1100) {
			t.Fatalf("Add = %v, %v", sum, err)
		}
	})

	t.Run("MulBasisPoints", func(t *testing.T) {
		t.Parallel()
		if got := eur(t, 10_000).MulBasisPoints(250); got != eur(t, 250) {
			t.Fatalf("2.5%% of 10000 = %v, want 250", got)
		}
	})

	t.Run("bidding window requires end after start", func(t *testing.T) {
		t.Parallel()
		if _, err := auction.NewBiddingWindow(t0, t0); !errors.Is(err, auction.ErrInvalidBiddingWindow) {
			t.Fatalf("err = %v, want ErrInvalidBiddingWindow", err)
		}
	})

	t.Run("reserve must be positive and zero value means none", func(t *testing.T) {
		t.Parallel()
		if _, err := auction.NewReservePrice(auction.Money{}); !errors.Is(err, auction.ErrInvalidReserve) {
			t.Fatalf("err = %v, want ErrInvalidReserve", err)
		}
		if !auction.NoReserve().IsZero() {
			t.Fatal("NoReserve must be zero")
		}
		if !auction.NoReserve().MetBy(eur(t, 1)) {
			t.Fatal("no reserve must be met by any amount")
		}
	})

	t.Run("status and outcome closed enums", func(t *testing.T) {
		t.Parallel()
		s, err := auction.NewStatusFromString("listed")
		if err != nil || s != auction.StatusListed {
			t.Fatalf("status = %v, %v", s, err)
		}
		if _, err := auction.NewStatusFromString("bogus"); !errors.Is(err, auction.ErrInvalidStatus) {
			t.Fatalf("err = %v, want ErrInvalidStatus", err)
		}
		o, err := auction.NewOutcomeFromString("sold")
		if err != nil || o != auction.OutcomeSold {
			t.Fatalf("outcome = %v, %v", o, err)
		}
		if _, err := auction.NewOutcomeFromString(""); !errors.Is(err, auction.ErrInvalidOutcome) {
			t.Fatalf("err = %v, want ErrInvalidOutcome", err)
		}
		r, err := auction.NewFailureReasonFromString("payment_timeout")
		if err != nil || r != auction.ReasonPaymentTimeout {
			t.Fatalf("reason = %v, %v", r, err)
		}
		if _, err := auction.NewFailureReasonFromString("oops"); !errors.Is(err, auction.ErrInvalidFailureReason) {
			t.Fatalf("err = %v, want ErrInvalidFailureReason", err)
		}
	})

	t.Run("anti-snipe policy validation", func(t *testing.T) {
		t.Parallel()
		if _, err := auction.NewAntiSnipePolicy(0, time.Minute, 1); !errors.Is(err, auction.ErrInvalidAntiSnipePolicy) {
			t.Fatalf("err = %v, want ErrInvalidAntiSnipePolicy", err)
		}
		if _, err := auction.NewAntiSnipePolicy(time.Minute, 0, 1); !errors.Is(err, auction.ErrInvalidAntiSnipePolicy) {
			t.Fatalf("err = %v, want ErrInvalidAntiSnipePolicy", err)
		}
		if _, err := auction.NewAntiSnipePolicy(time.Minute, time.Minute, -1); !errors.Is(err, auction.ErrInvalidAntiSnipePolicy) {
			t.Fatalf("err = %v, want ErrInvalidAntiSnipePolicy", err)
		}
	})
}
