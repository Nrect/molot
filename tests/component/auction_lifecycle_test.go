//go:build component

package component_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/tests"
)

// TestAuctionLifecycle is the happy-path component test covering the end-
// to-end auction flow: register participants → verify → list auction →
// place bids → wait for the ClosingWorker to close the auction → assert
// AuctionClosed status and settlement saga start.
//
// The bidding window is set to ~3 seconds; the ClosingWorker polls every
// 200 ms, so the auction closes naturally without any sleep. assert.Eventually
// polls the auction card with a 200 ms tick (no sleep — rule 42).
func TestAuctionLifecycle(t *testing.T) {
	t.Parallel()

	c := sharedClient

	// --- participants --------------------------------------------------------
	sellerID := uuid.New()
	bidder1ID := uuid.New()
	bidder2ID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID,
		fmt.Sprintf("seller-%s@test.molot", sellerID), "Test Seller")
	c.RegisterParticipant(t, bidder1ID,
		fmt.Sprintf("bidder1-%s@test.molot", bidder1ID), "Bidder One")
	c.RegisterParticipant(t, bidder2ID,
		fmt.Sprintf("bidder2-%s@test.molot", bidder2ID), "Bidder Two")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	bidder1Token := c.FakeBidderJWT(t, bidder1ID)
	bidder2Token := c.FakeBidderJWT(t, bidder2ID)
	opsToken := c.FakeOperationsJWT(t, opsID)

	// Verify bidders so they can bid above the EUR 100 000 verify-above
	// threshold configured in NewComponentTestApplication.
	c.VerifyParticipant(t, bidder1ID, opsToken)
	c.VerifyParticipant(t, bidder2ID, opsToken)

	// --- list auction --------------------------------------------------------
	auctionID := uuid.New()
	now := time.Now().UTC()
	endsAt := now.Add(3 * time.Second) // short window; ClosingWorker closes it

	c.ListAuction(t, tests.ListAuctionRequest{
		ID:              auctionID,
		Title:           "Antique bronze hammer",
		StartPriceMinor: 100_000,
		IncrementMinor:  10_000,
		Currency:        "EUR",
		StartsAt:        now.Add(-time.Second), // already open
		EndsAt:          endsAt,
	}, sellerToken)

	// Confirm the auction is listed.
	card := c.AuctionCard(t, auctionID, bidder1Token)
	require.Equal(t, "listed", card.Status)
	require.Equal(t, int64(100_000), card.StartPriceMinor)

	// --- place bids ----------------------------------------------------------
	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID:       uuid.New(),
		AmountMinor: 100_000,
		Currency:    "EUR",
	}, bidder1Token)

	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID:       uuid.New(),
		AmountMinor: 110_000,
		Currency:    "EUR",
	}, bidder2Token)

	// Card reflects bid2 as current price after both bids.
	card = c.AuctionCard(t, auctionID, bidder1Token)
	require.Equal(t, int64(110_000), card.CurrentPriceMinor)
	require.Equal(t, 2, card.BidCount)
	require.NotNil(t, card.LeaderID, "leader must be set after bids")
	assert.Equal(t, bidder2ID, *card.LeaderID)

	// --- wait for ClosingWorker (polls every 200 ms) -------------------------
	assert.Eventually(t, func() bool {
		card = c.AuctionCard(t, auctionID, bidder1Token)
		return card.Status == "closed"
	}, 10*time.Second, 200*time.Millisecond,
		"auction was not closed by the worker within 10 s")

	require.NotNil(t, card.Outcome, "closed auction must have an outcome")
	assert.Equal(t, "sold", *card.Outcome)

	// --- settlement saga started by AuctionClosedV1 --------------------------
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		return s.State == "awaiting_payment" || s.State == "settled"
	}, 10*time.Second, 200*time.Millisecond,
		"settlement saga did not reach awaiting_payment within 10 s")

	s := c.GetSettlementStatus(t, auctionID, opsToken)
	assert.Equal(t, bidder2ID, s.WinnerID, "winner must be the highest bidder")
	assert.Equal(t, int64(110_000), s.HammerMinor)
	assert.Equal(t, "EUR", s.Currency)
}
