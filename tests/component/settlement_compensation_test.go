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
	"database/sql"
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

// buildApp assembles a second in-process application bound to the same
// Postgres DB as the shared suite (unique IDs prevent collisions) with
// the given PSPMode and paymentTerm, and starts its background workers.
// The returned client speaks to its httptest.Server.
func buildApp(t *testing.T, db *sql.DB, pspMode string, paymentTerm time.Duration) (
	*monolith.Application, *tests.Client,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	app, err := monolith.NewComponentTestApplication(
		ctx, db, testHS256Secret, testPlatformCurrency,
		paymentTerm, pspMode,
	)
	require.NoError(t, err, "assemble secondary test application")

	go func() {
		if wErr := app.RunWorkers(ctx); wErr != nil && ctx.Err() == nil {
			t.Logf("secondary RunWorkers: %v", wErr)
		}
	}()

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

// getCompDB opens a DB connection from COMPONENT_DATABASE_URL for tests that
// assemble a second application instance. The connection is closed via
// t.Cleanup.
func getCompDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPONENT_DATABASE_URL")
	require.NotEmpty(t, dsn, "COMPONENT_DATABASE_URL must be set")
	db, err := monolith.NewDB(context.Background(), dsn)
	require.NoError(t, err, "open component DB")
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSettlementCompensation_RunnerUp: winner's invoice expires (the winner
// does not pay within the short payment term) → second-chance offer issued
// to the qualified runner-up → runner-up pays → saga Settled.
func TestSettlementCompensation_RunnerUp(t *testing.T) {
	t.Parallel()

	// Very short payment term so the ExpiryWorker fires quickly.
	const shortPaymentTerm = 3 * time.Second

	db := getCompDB(t)
	_, c := buildApp(t, db, billingadapters.PSPModeSuccess, shortPaymentTerm)

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
	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, runnerUpToken)

	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 110_000, Currency: "EUR",
	}, winnerToken)

	assert.Eventually(t, func() bool {
		return c.AuctionCard(t, auctionID, winnerToken).Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed")

	// saga issues invoice for the winner.
	var firstInvoiceID uuid.UUID
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		if s.State == "awaiting_payment" && s.InvoiceID != nil {
			firstInvoiceID = *s.InvoiceID
			return true
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "saga did not reach awaiting_payment")

	// The winner intentionally does NOT pay — invoice expires after shortPaymentTerm.
	_ = firstInvoiceID

	// --- invoice expires → saga awards runner-up → SecondChancePayment -------
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		return s.State == "second_chance_payment"
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
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		return s.State == "settled"
	}, 10*time.Second, 200*time.Millisecond, "saga did not settle after runner-up payment")

	inv := c.GetInvoice(t, secondInvoiceID, runnerUpToken)
	assert.Equal(t, "paid", inv.Status)
	assert.Equal(t, 2, inv.Attempt)
}

// TestSettlementCompensation_CapRelist: winner's invoice expires (no runner-up
// qualifies) → Relist (gen=0); relisted auction also fails → FailedUnsold
// (gen=1 → cap reached, no further relists).
func TestSettlementCompensation_CapRelist(t *testing.T) {
	t.Parallel()

	const shortPaymentTerm = 2 * time.Second

	db := getCompDB(t)
	_, c := buildApp(t, db, billingadapters.PSPModeSuccess, shortPaymentTerm)

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
	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, winnerToken)

	assert.Eventually(t, func() bool {
		return c.AuctionCard(t, auctionID, winnerToken).Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed")

	// Winner does not pay; invoice expires → no runner-up → Relist (gen=0).
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		return s.State == "relisted"
	}, 20*time.Second, 200*time.Millisecond, "saga did not reach relisted (gen 0)")

	s := c.GetSettlementStatus(t, auctionID, opsToken)
	require.NotNil(t, s.FailureReason)
	assert.Equal(t, "payment_timeout", *s.FailureReason)

	// --- find the relisted auction in the dashboard --------------------------
	var newAuctionID uuid.UUID
	assert.Eventually(t, func() bool {
		dash := c.SellerDashboard(t, sellerID, sellerToken)
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
	assert.Eventually(t, func() bool {
		card := c.AuctionCard(t, newAuctionID, winnerToken)
		return card.Status == "listed"
	}, 10*time.Second, 200*time.Millisecond, "relisted auction not listed yet")

	c.PlaceBid(t, newAuctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, winnerToken)

	// Wait for the relisted auction to close. RelistDuration=30s by default;
	// the closing worker will close it after the window expires.
	assert.Eventually(t, func() bool {
		card := c.AuctionCard(t, newAuctionID, winnerToken)
		return card.Status == "closed"
	}, 45*time.Second, 500*time.Millisecond, "relisted auction not closed within 45 s")

	// Winner does not pay again → invoice expires → relistGen==1 → FailedUnsold.
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, newAuctionID, opsToken)
		return s.State == "failed_unsold"
	}, 20*time.Second, 200*time.Millisecond,
		"relisted auction saga did not reach failed_unsold (cap-relist)")

	s2 := c.GetSettlementStatus(t, newAuctionID, opsToken)
	require.NotNil(t, s2.FailureReason)
	assert.Equal(t, "payment_timeout", *s2.FailureReason)
}
