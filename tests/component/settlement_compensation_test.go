//go:build component

// settlement_compensation_test.go covers the two compensation forks of the
// settlement saga (ARCHITECTURE §6, §12):
//
//  1. RunnerUp second-chance: winner's invoice expires (PSP_MODE=success
//     but the winner never calls PayInvoice, so the invoice times out) →
//     runner-up gets a second-chance offer → runner-up pays → Settled.
//
//  2. cap-relist: winner's payment times out, no runner-up qualifies → Relist
//     (gen=0); relisted auction closes, winner times out again → FailedUnsold
//     (gen=1, cap reached).
//
// Both tests use a very short PAYMENT_TERM so the ExpiryWorker fires quickly
// without long sleeps. Tests assemble their own in-process applications so
// the PSP mode / payment term can be tuned independently of the shared suite.
package component_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billingadapters "molot/internal/billing/adapters"
	"molot/internal/monolith"
	"molot/tests"
)

// buildApp assembles a second in-process application with the given PSPMode
// and paymentTerm and starts its background workers. The returned client
// speaks to its httptest.Server.
//
// Each secondary application gets its OWN database (created here, dropped on
// cleanup). Sharing one DB is not an option: consumer-group names are equal
// across application instances, so two apps on one DB would compete for the
// same messages while carrying different configs — e.g. the shared app's saga
// could issue an invoice for this test's auction with the wrong payment term.
func buildApp(t *testing.T, pspMode string, paymentTerm time.Duration) (
	*monolith.Application, *tests.Client,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	adminDSN := os.Getenv("COMPONENT_DATABASE_URL")
	require.NotEmpty(t, adminDSN, "COMPONENT_DATABASE_URL must be set")

	db, cleanupDB, err := createIsolatedDB(ctx, adminDSN)
	require.NoError(t, err, "create isolated database")

	app, err := monolith.NewComponentTestApplication(
		ctx, db, testHS256Secret, testPlatformCurrency,
		paymentTerm, pspMode,
	)
	if err != nil {
		cleanupDB()
		require.NoError(t, err, "assemble secondary test application")
	}

	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		if wErr := app.RunWorkers(ctx); wErr != nil && ctx.Err() == nil {
			t.Logf("secondary RunWorkers: %v", wErr)
		}
	}()

	// Ordered teardown: cancel → wait for RunWorkers to exit (the router
	// drains in-flight handlers) → close the pool and drop the database.
	// Without the wait, subscribers keep polling a closed pool / dropped
	// database and spam teardown errors into the next test's log window.
	t.Cleanup(func() {
		cancel()
		select {
		case <-workersDone:
		case <-time.After(10 * time.Second):
			t.Log("secondary RunWorkers did not exit within 10 s; dropping DB anyway")
		}
		cleanupDB()
	})

	srv := httptest.NewServer(app.HTTPHandler)
	t.Cleanup(srv.Close)

	client := tests.NewClient(srv.URL, testHS256Secret)

	// Poll /readyz until the router is running.
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(srv.URL + "/readyz")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return app, client
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("readyz did not return 200 within 15 s for secondary app")
	return nil, nil
}

// TestSettlementCompensation_RunnerUp: winner's invoice expires (the winner
// does not pay within the short payment term) → second-chance offer issued
// to the qualified runner-up → runner-up pays → saga Settled.
func TestSettlementCompensation_RunnerUp(t *testing.T) {
	// Very short payment term so the ExpiryWorker fires quickly.
	const shortPaymentTerm = 3 * time.Second

	_, c := buildApp(t, billingadapters.PSPModeSuccess, shortPaymentTerm)

	// --- participants --------------------------------------------------------
	sellerID := uuid.New()
	winnerID := uuid.New()
	runnerUpID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID, fmt.Sprintf("sc-seller-%s@m", sellerID), "SC Seller")
	c.RegisterParticipant(t, winnerID, fmt.Sprintf("sc-winner-%s@m", winnerID), "SC Winner")
	c.RegisterParticipant(t, runnerUpID, fmt.Sprintf("sc-runner-%s@m", runnerUpID), "SC Runner")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	winnerToken := c.FakeBidderJWT(t, winnerID)
	runnerUpToken := c.FakeBidderJWT(t, runnerUpID)
	opsToken := c.FakeOperationsJWT(t, opsID)

	c.VerifyParticipant(t, winnerID, opsToken)
	c.VerifyParticipant(t, runnerUpID, opsToken)

	// --- list and close auction ----------------------------------------------
	auctionID := uuid.New()
	now := time.Now().UTC()

	c.ListAuction(t, tests.ListAuctionRequest{
		ID: auctionID, Title: "SC Runner-Up Lot",
		StartPriceMinor: 100_000, IncrementMinor: 10_000, Currency: "EUR",
		StartsAt: now.Add(-time.Second), EndsAt: now.Add(3 * time.Second),
	}, sellerToken)

	// runner-up bids first, winner outbids — runner-up qualifies because it
	// bid at the start price which is >= reserve (no reserve here, always met).
	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, runnerUpToken)

	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 110_000, Currency: "EUR",
	}, winnerToken)

	// Poll variants only inside Eventually: require.* in the condition
	// goroutine Goexits it and freezes Eventually (see tests/client.go).
	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
		return ok && card.Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed")

	// saga issues invoice for the winner.
	var firstInvoiceID uuid.UUID
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		if ok && s.State == "awaiting_payment" && s.InvoiceID != nil {
			firstInvoiceID = *s.InvoiceID
			return true
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "saga did not reach awaiting_payment")

	// The winner intentionally does NOT pay — invoice expires after shortPaymentTerm.
	_ = firstInvoiceID

	// --- invoice expires → saga awards runner-up → SecondChancePayment -------
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		return ok && s.State == "second_chance_payment"
	}, 20*time.Second, 200*time.Millisecond, "saga did not reach second_chance_payment")

	s := c.GetSettlementStatus(t, auctionID, opsToken)
	require.NotNil(t, s.InvoiceID, "second-chance invoice must be set")
	secondInvoiceID := *s.InvoiceID
	assert.Equal(t, runnerUpID, s.WinnerID, "winner must be reassigned to runner-up")
	assert.Equal(t, 2, s.Attempt)

	// --- runner-up pays the second-chance invoice ----------------------------
	c.PayInvoice(t, secondInvoiceID, runnerUpToken)

	// --- saga settles --------------------------------------------------------
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		return ok && s.State == "settled"
	}, 10*time.Second, 200*time.Millisecond, "saga did not settle after runner-up payment")

	inv := c.GetInvoice(t, secondInvoiceID, runnerUpToken)
	assert.Equal(t, "paid", inv.Status)
	assert.Equal(t, 2, inv.Attempt)
}

// TestSettlementCompensation_CapRelist: winner's invoice expires (no runner-up
// qualifies) → Relist (gen=0); relisted auction also fails → FailedUnsold
// (gen=1 → cap reached, no further relists).
func TestSettlementCompensation_CapRelist(t *testing.T) {
	const shortPaymentTerm = 2 * time.Second

	_, c := buildApp(t, billingadapters.PSPModeSuccess, shortPaymentTerm)

	// --- participants --------------------------------------------------------
	sellerID := uuid.New()
	winnerID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID, fmt.Sprintf("cr-seller-%s@m", sellerID), "CR Seller")
	c.RegisterParticipant(t, winnerID, fmt.Sprintf("cr-winner-%s@m", winnerID), "CR Winner")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	winnerToken := c.FakeBidderJWT(t, winnerID)
	opsToken := c.FakeOperationsJWT(t, opsID)
	c.VerifyParticipant(t, winnerID, opsToken)

	// --- list and close (single bidder — no runner-up) -----------------------
	auctionID := uuid.New()
	now := time.Now().UTC()

	c.ListAuction(t, tests.ListAuctionRequest{
		ID: auctionID, Title: "Cap-Relist Lot",
		StartPriceMinor: 100_000, IncrementMinor: 10_000, Currency: "EUR",
		StartsAt: now.Add(-time.Second), EndsAt: now.Add(3 * time.Second),
	}, sellerToken)

	// Single bidder only → no runner-up qualifies (runnerUpBid.IsZero()).
	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, winnerToken)

	// Poll variants only inside Eventually: require.* in the condition
	// goroutine Goexits it and freezes Eventually (see tests/client.go).
	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
		return ok && card.Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed")

	// Winner does not pay; invoice expires → no runner-up → Relist (gen=0).
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		return ok && s.State == "relisted"
	}, 20*time.Second, 200*time.Millisecond, "saga did not reach relisted (gen 0)")

	s := c.GetSettlementStatus(t, auctionID, opsToken)
	require.NotNil(t, s.FailureReason)
	assert.Equal(t, "payment_timeout", *s.FailureReason)

	// --- find the relisted auction in the dashboard --------------------------
	var newAuctionID uuid.UUID
	assert.Eventually(t, func() bool {
		dash, ok := c.SellerDashboardPoll(t, sellerID, sellerToken)
		if !ok {
			return false
		}
		for _, item := range dash.Items {
			if item.AuctionID != auctionID && item.Status == "listed" {
				newAuctionID = item.AuctionID
				return true
			}
		}
		return false
	}, 15*time.Second, 200*time.Millisecond, "relisted auction not found in dashboard")

	require.NotEqual(t, uuid.Nil, newAuctionID)

	// Bid on the relisted auction so it closes as Sold (needed for a settlement).
	// The bidding window opens at startsAt = relist time + RelistDelay, so wait
	// for the window, not just for the row to appear (the card projection for
	// the relisted auction may not exist yet — poll tolerates the 404).
	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, newAuctionID, winnerToken)
		return ok && card.Status == "listed" && !card.StartsAt.After(time.Now().UTC())
	}, 10*time.Second, 200*time.Millisecond, "relisted auction not open for bidding yet")

	c.PlaceBid(t, newAuctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, winnerToken)

	// Wait for the relisted auction to close. RelistDuration=6s in the component
	// config; the closing worker closes it after the window expires.
	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, newAuctionID, winnerToken)
		return ok && card.Status == "closed"
	}, 45*time.Second, 500*time.Millisecond, "relisted auction not closed within 45 s")

	// Winner does not pay again → invoice expires → relistGen==1 → FailedUnsold.
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, newAuctionID, opsToken)
		return ok && s.State == "failed_unsold"
	}, 20*time.Second, 200*time.Millisecond,
		"relisted auction saga did not reach failed_unsold (cap-relist)")

	s2 := c.GetSettlementStatus(t, newAuctionID, opsToken)
	require.NotNil(t, s2.FailureReason)
	assert.Equal(t, "payment_timeout", *s2.FailureReason)
}
