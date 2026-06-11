//go:build e2e

// E2E level (docs/test-taxonomy.md, ARCHITECTURE §12): one short
// critical-path flow against the PRODUCTION binaries from docker compose,
// through the public HTTP port only. It proves wiring and contracts —
// config parsing, migrations, the SQL bus, both workers, the saga and the
// HTTP stack assembled exactly as in production — not business logic:
// corner cases are owned by the lower test levels.
//
// Setup (or simply `make test-e2e`):
//
//	SNIPE_WINDOW=1s SNIPE_EXTENSION=1s docker compose up -d --build --wait
//	E2E_BASE_URL=http://localhost:8080 go test -race -tags e2e ./tests/...
//
// The SNIPE_* overrides shrink the anti-snipe window (interpolated into the
// app environment by docker-compose.yml): with the production-like 5m
// policy from .env.example every bid on a seconds-long e2e lot would extend
// the deadline by minutes.
//
// Gate: the test skips itself when E2E_BASE_URL is unset.
package e2e_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/tests"
)

const (
	// devSecret mirrors AUTH_HS256_SECRET in .env.example — the env file
	// docker-compose feeds to the app service (AUTH_MODE=local-hs256), so
	// generated tokens validate against the running container.
	devSecret = "dev-secret-change-me"
	// currency mirrors PLATFORM_CURRENCY in .env.example.
	currency = "USD"
	// Eventually budgets are deliberately generous: the compose stack
	// (prod binaries, SQL bus, polling workers) converges slower than the
	// in-process httptest setup of the component level.
	waitBudget = 30 * time.Second
	pollTick   = 250 * time.Millisecond
)

// TestCriticalPath drives the hammer-to-settlement flow end to end:
// register participants → operations verifies the bidders → seller lists a
// short-lived lot → two bids → ClosingWorker hammers the lot → the
// settlement saga issues an invoice → the winner pays → the saga settles.
func TestCriticalPath(t *testing.T) {
	baseURL := os.Getenv("E2E_BASE_URL")
	if baseURL == "" {
		t.Skip("E2E_BASE_URL not set; skipping e2e tests (run `make test-e2e`)")
	}

	c := tests.NewClient(baseURL, devSecret)

	// The app container publishes the port before /readyz turns green
	// (migrations + bus startup) — wait for readiness, not just the socket.
	c.WaitForReadyz(t, waitBudget)

	// --- register and verify participants ------------------------------------
	sellerID := uuid.New()
	winnerID := uuid.New()
	runnerUpID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID,
		fmt.Sprintf("e2e-seller-%s@molot.test", sellerID), "E2E Seller")
	c.RegisterParticipant(t, winnerID,
		fmt.Sprintf("e2e-winner-%s@molot.test", winnerID), "E2E Winner")
	c.RegisterParticipant(t, runnerUpID,
		fmt.Sprintf("e2e-runner-%s@molot.test", runnerUpID), "E2E Runner-up")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	winnerToken := c.FakeBidderJWT(t, winnerID)
	runnerUpToken := c.FakeBidderJWT(t, runnerUpID)
	opsToken := c.FakeOperationsJWT(t, opsID)

	c.VerifyParticipant(t, winnerID, opsToken)
	c.VerifyParticipant(t, runnerUpID, opsToken)

	// --- list a short-lived lot -----------------------------------------------
	auctionID := uuid.New()
	now := time.Now().UTC()

	c.ListAuction(t, tests.ListAuctionRequest{
		ID:              auctionID,
		Title:           "E2E critical path lot",
		StartPriceMinor: 100_000,
		IncrementMinor:  10_000,
		Currency:        currency,
		StartsAt:        now.Add(-time.Second), // already open
		EndsAt:          now.Add(8 * time.Second),
	}, sellerToken)

	// --- two bids ---------------------------------------------------------------
	// Amounts at/above VERIFY_ABOVE_MINOR (100000 in .env.example) so the
	// verification wiring is exercised end to end. The first qualifying bid
	// per bidder retries while the bidder-profile projection catches up with
	// the just-issued verification.
	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: currency,
	}, runnerUpToken)
	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 110_000, Currency: currency,
	}, winnerToken)

	// --- ClosingWorker hammers the lot ----------------------------------------
	// Poll variants only inside Eventually: require.* in the condition
	// goroutine Goexits it and freezes Eventually (see tests/client.go).
	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
		return ok && card.Status == "closed"
	}, waitBudget, pollTick, "auction was not closed by the worker")

	// --- saga issues the invoice (awaiting_payment) ----------------------------
	var invoiceID uuid.UUID
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		if ok && s.State == "awaiting_payment" && s.InvoiceID != nil {
			invoiceID = *s.InvoiceID
			return true
		}
		return false
	}, waitBudget, pollTick, "saga did not reach awaiting_payment")
	require.NotEqual(t, uuid.Nil, invoiceID,
		"settlement must reference the issued invoice")

	// --- winner pays; saga settles ---------------------------------------------
	c.PayInvoice(t, invoiceID, winnerToken)

	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		return ok && s.State == "settled"
	}, waitBudget, pollTick, "saga did not settle after payment")

	// Final wiring sanity: contracts round-trip through the public API.
	s := c.GetSettlementStatus(t, auctionID, opsToken)
	assert.Equal(t, winnerID, s.WinnerID, "winner must be the highest bidder")
	assert.Equal(t, int64(110_000), s.HammerMinor)
	assert.Equal(t, currency, s.Currency)
}
