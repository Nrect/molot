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

// TestSettlementHappy covers the nominal settlement path:
//
//   AuctionClosed (sold) → saga.Started → IssueInvoice →
//   saga.AwaitingPayment → winner pays invoice →
//   InvoicePaid → saga.Settled → auction.SaleSettled →
//   SaleSettledV1 on seller dashboard
//
// PSP_MODE=success (the shared application default) so every PayInvoice
// succeeds on the first call.
func TestSettlementHappy(t *testing.T) {
	t.Parallel()

	c := sharedClient

	// --- bootstrap participants ----------------------------------------------
	sellerID := uuid.New()
	winnerID := uuid.New()
	runnerUpID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID,
		fmt.Sprintf("sh-seller-%s@m", sellerID), "SH Seller")
	c.RegisterParticipant(t, winnerID,
		fmt.Sprintf("sh-winner-%s@m", winnerID), "SH Winner")
	c.RegisterParticipant(t, runnerUpID,
		fmt.Sprintf("sh-runner-%s@m", runnerUpID), "SH Runner")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	winnerToken := c.FakeBidderJWT(t, winnerID)
	runnerUpToken := c.FakeBidderJWT(t, runnerUpID)
	opsToken := c.FakeOperationsJWT(t, opsID)

	c.VerifyParticipant(t, winnerID, opsToken)
	c.VerifyParticipant(t, runnerUpID, opsToken)

	// --- list auction --------------------------------------------------------
	auctionID := uuid.New()
	now := time.Now().UTC()

	c.ListAuction(t, tests.ListAuctionRequest{
		ID:              auctionID,
		Title:           "Settlement Happy Test Lot",
		StartPriceMinor: 100_000,
		IncrementMinor:  10_000,
		Currency:        "EUR",
		StartsAt:        now.Add(-time.Second),
		EndsAt:          now.Add(3 * time.Second),
	}, sellerToken)

	// runner-up bids first, winner outbids.
	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, runnerUpToken)
	c.PlaceBid(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 110_000, Currency: "EUR",
	}, winnerToken)

	// Wait for ClosingWorker.
	assert.Eventually(t, func() bool {
		return c.AuctionCard(t, auctionID, winnerToken).Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed in time")

	// --- saga reaches AwaitingPayment ----------------------------------------
	var invoiceID uuid.UUID
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		if s.State == "awaiting_payment" && s.InvoiceID != nil {
			invoiceID = *s.InvoiceID
			return true
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "saga did not reach awaiting_payment")

	require.NotEqual(t, uuid.Nil, invoiceID, "invoice ID must be set in settlement")

	// --- winner fetches and pays invoice -------------------------------------
	inv := c.GetInvoice(t, invoiceID, winnerToken)
	require.Equal(t, "pending", inv.Status)
	assert.Equal(t, int64(110_000), inv.HammerMinor)
	assert.Equal(t, "EUR", inv.Currency)
	// Commission at 1000bp = 10 %
	assert.Equal(t, int64(11_000), inv.CommissionMinor)
	assert.Equal(t, int64(121_000), inv.TotalMinor)
	assert.Equal(t, 1, inv.Attempt)

	c.PayInvoice(t, invoiceID, winnerToken)

	// --- saga settles --------------------------------------------------------
	assert.Eventually(t, func() bool {
		s := c.GetSettlementStatus(t, auctionID, opsToken)
		return s.State == "settled"
	}, 10*time.Second, 200*time.Millisecond, "saga did not settle")

	// Invoice is now paid.
	inv = c.GetInvoice(t, invoiceID, winnerToken)
	assert.Equal(t, "paid", inv.Status)

	// --- auction reflects SaleSettled on the seller dashboard ---------------
	assert.Eventually(t, func() bool {
		dash := c.SellerDashboard(t, sellerID, sellerToken)
		for _, item := range dash.Items {
			if item.AuctionID == auctionID {
				return item.SettlementStatus != nil &&
					*item.SettlementStatus == "settled"
			}
		}
		return false
	}, 10*time.Second, 200*time.Millisecond,
		"SaleSettledV1 not reflected in seller dashboard")

	// Final dashboard state check.
	dash := c.SellerDashboard(t, sellerID, sellerToken)
	var found bool
	for _, item := range dash.Items {
		if item.AuctionID == auctionID {
			found = true
			require.NotNil(t, item.SettlementStatus)
			assert.Equal(t, "settled", *item.SettlementStatus)
			assert.Equal(t, int64(110_000), item.HammerPriceMinor)
		}
	}
	assert.True(t, found, "auction not found in seller dashboard")
}
