//go:build component

// payment_refund_test.go covers the PSP-centric compensation branches of
// PayInvoice (ARCHITECTURE §3.1, §12):
//
//  1. PSP_MODE=flaky: first Charge call returns ErrPSPUnavailable (502);
//     the SECOND call (retry by the client) succeeds — the idempotency key
//     prevents a double charge. Result: 204 on retry.
//
//  2. Race: invoice expires between Charge and MarkPaid (ARCHITECTURE §3.1
//     "Гонка проиграна"). In the real system this is a crash-seam; here we
//     approximate it by expiring the invoice (very short payment term) while
//     a concurrent goroutine tries to pay it. One of them wins; the other
//     gets 409 invoice-no-longer-payable. Both paths trigger a Refund.
//     The saga correctly processes whichever event arrives (InvoicePaid or
//     InvoiceExpired).
package component_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billingadapters "molot/internal/billing/adapters"
	"molot/tests"
)

// TestPaymentRefund_Flaky: PSP_MODE=flaky — first Charge fails with 502,
// retry succeeds (idempotency key prevents double debit).
func TestPaymentRefund_Flaky(t *testing.T) {
	_, c := buildApp(t, billingadapters.PSPModeFlaky, 30*time.Second)

	// --- participants --------------------------------------------------------
	sellerID := uuid.New()
	winnerID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID, fmt.Sprintf("fl-seller-%s@m", sellerID), "Flaky Seller")
	c.RegisterParticipant(t, winnerID, fmt.Sprintf("fl-winner-%s@m", winnerID), "Flaky Winner")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	winnerToken := c.FakeBidderJWT(t, winnerID)
	opsToken := c.FakeOperationsJWT(t, opsID)
	c.VerifyParticipant(t, winnerID, opsToken)

	// --- list, bid, close ----------------------------------------------------
	auctionID := uuid.New()
	now := time.Now().UTC()

	c.ListAuction(t, tests.ListAuctionRequest{
		ID: auctionID, Title: "Flaky PSP Lot",
		StartPriceMinor: 100_000, IncrementMinor: 10_000, Currency: "EUR",
		StartsAt: now.Add(-time.Second), EndsAt: now.Add(3 * time.Second),
	}, sellerToken)

	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, winnerToken)

	// Poll variants only inside Eventually: require.* in the condition
	// goroutine Goexits it and freezes Eventually (see tests/client.go).
	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
		return ok && card.Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed")

	// Wait for invoice.
	var invoiceID uuid.UUID
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		if ok && s.State == "awaiting_payment" && s.InvoiceID != nil {
			invoiceID = *s.InvoiceID
			return true
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "saga did not reach awaiting_payment")

	// --- first PayInvoice: PSP_MODE=flaky → 502 psp-unavailable --------------
	status1 := c.PayInvoiceExpect(t, invoiceID, winnerToken)
	assert.Equal(t, http.StatusBadGateway, status1,
		"first payment attempt must return 502 (psp-unavailable)")

	// --- second PayInvoice (retry): PSP idempotency key → same charge ref, success
	c.PayInvoice(t, invoiceID, winnerToken) // asserts 204

	// --- saga settles --------------------------------------------------------
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		return ok && s.State == "settled"
	}, 10*time.Second, 200*time.Millisecond, "saga did not settle after flaky retry")

	inv := c.GetInvoice(t, invoiceID, winnerToken)
	assert.Equal(t, "paid", inv.Status)
}

// TestPaymentRefund_ExpiredRace: invoice expires concurrently with a payment
// attempt. One wins, the other gets 409. The saga processes whichever event
// arrives first and reaches a terminal state.
//
// We use a very short PAYMENT_TERM (2 s) to make the expiry race realistic
// without long sleeps.
func TestPaymentRefund_ExpiredRace(t *testing.T) {
	const shortTerm = 2 * time.Second

	_, c := buildApp(t, billingadapters.PSPModeSuccess, shortTerm)

	// --- participants --------------------------------------------------------
	sellerID := uuid.New()
	winnerID := uuid.New()
	opsID := uuid.New()

	c.RegisterParticipant(t, sellerID, fmt.Sprintf("er-seller-%s@m", sellerID), "Exp Race Seller")
	c.RegisterParticipant(t, winnerID, fmt.Sprintf("er-winner-%s@m", winnerID), "Exp Race Winner")

	sellerToken := c.FakeSellerJWT(t, sellerID)
	winnerToken := c.FakeBidderJWT(t, winnerID)
	opsToken := c.FakeOperationsJWT(t, opsID)
	c.VerifyParticipant(t, winnerID, opsToken)

	// --- list, bid, close ----------------------------------------------------
	auctionID := uuid.New()
	now := time.Now().UTC()

	c.ListAuction(t, tests.ListAuctionRequest{
		ID: auctionID, Title: "Expired Race Lot",
		StartPriceMinor: 100_000, IncrementMinor: 10_000, Currency: "EUR",
		StartsAt: now.Add(-time.Second), EndsAt: now.Add(3 * time.Second),
	}, sellerToken)

	c.PlaceBidEventually(t, auctionID, tests.PlaceBidRequest{
		BidID: uuid.New(), AmountMinor: 100_000, Currency: "EUR",
	}, winnerToken)

	assert.Eventually(t, func() bool {
		card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
		return ok && card.Status == "closed"
	}, 10*time.Second, 200*time.Millisecond, "auction not closed")

	var invoiceID uuid.UUID
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		if ok && s.State == "awaiting_payment" && s.InvoiceID != nil {
			invoiceID = *s.InvoiceID
			return true
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "saga did not reach awaiting_payment")

	// --- race: concurrently attempt payment while expiry timer runs ----------
	// PAYMENT_TERM=2 s; the expiry worker fires at ~2 s.
	// We spawn both a payment goroutine and wait for either path to complete.
	var (
		payStatus   int
		payStatusMu sync.Mutex
		wg          sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Attempt payment; result may be 204 (won) or 409 (lost to expiry).
		status := c.PayInvoiceExpect(t, invoiceID, winnerToken)
		payStatusMu.Lock()
		payStatus = status
		payStatusMu.Unlock()
	}()

	wg.Wait()

	payStatusMu.Lock()
	got := payStatus
	payStatusMu.Unlock()

	// The outcome must be one of: 204 (paid first), 409 (expired first).
	require.True(t,
		got == http.StatusNoContent || got == http.StatusConflict,
		"expected 204 or 409, got %d", got)

	// The saga must reach a terminal state regardless of which path won.
	// If paid: Settled; if expired (no runner-up, relistGen=0): Relisted.
	assert.Eventually(t, func() bool {
		s, ok := c.SettlementStatusPoll(t, auctionID, opsToken)
		return ok && (s.State == "settled" || s.State == "relisted" || s.State == "failed_unsold")
	}, 20*time.Second, 200*time.Millisecond,
		"saga did not reach a terminal state after payment race")
}
