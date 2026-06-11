package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/settlement/domain/settlement"
)

// The saga event handlers are tested only for orchestration (rule 41):
// recording spy gateways with programmable "already done" outcomes plus
// an in-memory repository. Every crash seam of §6.7 has an
// idempotent-continuation test: the redelivery after the failure point
// drives the saga to the correct final state.

func TestOnAuctionClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("sold auction starts the saga and issues the first invoice", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})

		require.NoError(t, f.handlers.OnAuctionClosed(ctx, e))

		saga := f.repo.mustGet(t, auctionIDOf(t, e))
		assert.Equal(t, settlement.StateAwaitingPayment, saga.State())
		require.Len(t, f.billing.issues, 1)
		issue := f.billing.issues[0]
		assert.Equal(t, firstInvoiceOf(t, e), issue.invoiceID, "the deterministic attempt-1 invoice id")
		assert.Equal(t, e.WinnerID, issue.debtor.UUID())
		assert.Equal(t, e.HammerMinor, issue.amount.Amount())
		assert.Equal(t, 1, issue.attempt)
	})

	t.Run("not sold auction starts nothing", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		e.Outcome = "not_sold"
		e.WinnerID = [16]byte{} // not_sold events carry no winner

		require.NoError(t, f.handlers.OnAuctionClosed(ctx, e))
		assert.Empty(t, f.billing.issues)
		assert.Zero(t, f.repo.addCalls)
	})

	t.Run("malformed outcome is a poison message", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		e.Outcome = "withdrawn"
		assert.Error(t, f.handlers.OnAuctionClosed(ctx, e))
	})

	t.Run("redelivery after the saga advanced is acked without effects", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		f.closeSold(t, e)

		require.NoError(t, f.handlers.OnAuctionClosed(ctx, e))
		assert.Len(t, f.billing.issues, 1, "no duplicate IssueInvoice")
	})

	t.Run("seam: crash after the insert before IssueInvoice", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})

		// First delivery dies in the effect: the saga row exists, the
		// invoice does not.
		f.billing.issueErr = errors.New("billing is down")
		require.Error(t, f.handlers.OnAuctionClosed(ctx, e))
		assert.Equal(t, settlement.StateStarted, f.repo.mustGet(t, auctionIDOf(t, e)).State())

		// Redelivery continues from Started: issue → commit (§6.7).
		f.billing.issueErr = nil
		require.NoError(t, f.handlers.OnAuctionClosed(ctx, e))
		assert.Equal(t, settlement.StateAwaitingPayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("seam: crash after IssueInvoice before the commit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})

		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnAuctionClosed(ctx, e))
		require.Len(t, f.billing.issues, 1)
		assert.Equal(t, settlement.StateStarted, f.repo.mustGet(t, auctionIDOf(t, e)).State())

		// Redelivery repeats the effect (already-issued → nil on the
		// billing side) and completes the commit.
		f.repo.updateErr = nil
		require.NoError(t, f.handlers.OnAuctionClosed(ctx, e))
		assert.Len(t, f.billing.issues, 2, "the repeated effect is a no-op by the deterministic id")
		assert.Equal(t, settlement.StateAwaitingPayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})
}

func TestOnInvoicePaid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("payment settles the saga", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		f.closeSold(t, e)

		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		assert.Equal(t, settlement.StateSettled, f.repo.mustGet(t, auctionIDOf(t, e)).State())
		assert.Equal(t, []settlement.AuctionID{auctionIDOf(t, e)}, f.auctions.confirms)
	})

	t.Run("fast-forward: payment at Started proves the invoice was issued", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		// Crash before the AwaitingPayment commit: saga stuck in Started.
		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnAuctionClosed(ctx, e))
		f.repo.updateErr = nil

		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Equal(t, settlement.StateSettled, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("seam: crash after ConfirmSettlement before the commit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		f.closeSold(t, e)

		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		require.Len(t, f.auctions.confirms, 1)

		f.repo.updateErr = nil
		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Len(t, f.auctions.confirms, 2, "repeat ConfirmSettlement is a no-op (ErrAlreadySettled → nil)")
		assert.Equal(t, settlement.StateSettled, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("duplicate delivery at Settled is acked without effects", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		f.closeSold(t, e)
		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Len(t, f.auctions.confirms, 1)
	})

	t.Run("a foreign invoice is acked without effects", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		f.closeSold(t, e)

		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, secondInvoiceOf(t, e).UUID()))
		assert.Empty(t, f.auctions.confirms)
		assert.Equal(t, settlement.StateAwaitingPayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("an unknown settlement is an anomaly, not an ack", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{})
		assert.Error(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
	})

	t.Run("seam: a concurrent commit between decide and commit is acked", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{noRunnerUp: true, relistGen: 1})
		f.closeSold(t, e)
		id := auctionIDOf(t, e)

		// A competing expiry handler commits FailedUnsold right before
		// our updateFn runs — the duplicate's commit must ack (§6.3 last row).
		f.repo.onUpdate = func() {
			f.repo.mutateLocked(t, id, func(s *settlement.Settlement) error {
				return s.ApplyNextStep(settlement.StepFailUnsold, settlement.ReasonPaymentTimeout)
			})
		}
		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Equal(t, settlement.StateFailedUnsold, f.repo.mustGet(t, id).State())
	})
}

func TestOnInvoiceExpired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("qualifying runner-up gets the second chance", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		assert.Equal(t, settlement.StateAwardingRunnerUp, f.repo.mustGet(t, auctionIDOf(t, e)).State())
		assert.Equal(t, []settlement.AuctionID{auctionIDOf(t, e)}, f.auctions.awards)
		assert.Empty(t, f.auctions.markFails)
	})

	t.Run("no qualifying runner-up relists the original once", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{noRunnerUp: true})
		f.closeSold(t, e)
		id := auctionIDOf(t, e)

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		saga := f.repo.mustGet(t, id)
		assert.Equal(t, settlement.StateRelisted, saga.State())
		assert.Equal(t, settlement.ReasonPaymentTimeout, saga.FailureReason())

		require.Len(t, f.auctions.markFails, 1)
		assert.Equal(t, settlement.ReasonPaymentTimeout, f.auctions.markFails[0].reason)
		require.Len(t, f.auctions.relists, 1)
		relist := f.auctions.relists[0]
		assert.Equal(t, id, relist.originalID)
		assert.NotEqual(t, id, relist.newID, "the replacement gets its own deterministic id")
		assert.Equal(t, testNow.Add(relistDelay), relist.startsAt)
		assert.Equal(t, testNow.Add(relistDelay+relistDuration), relist.endsAt)
	})

	t.Run("relist cap reached fails the sale for good", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{noRunnerUp: true, relistGen: 1})
		f.closeSold(t, e)

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		saga := f.repo.mustGet(t, auctionIDOf(t, e))
		assert.Equal(t, settlement.StateFailedUnsold, saga.State())
		assert.Len(t, f.auctions.markFails, 1)
		assert.Empty(t, f.auctions.relists, "no second relist past the cap")
	})

	t.Run("seam: crash between MarkSaleFailed and Relist", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{noRunnerUp: true})
		f.closeSold(t, e)

		f.auctions.relistErr = errors.New("crashed between the two effects")
		require.Error(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Equal(t, settlement.StateAwaitingPayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())

		// Redelivery repeats BOTH effects: FailSale → no-op, Relist runs.
		f.auctions.relistErr = nil
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Len(t, f.auctions.markFails, 2)
		assert.Len(t, f.auctions.relists, 2)
		assert.Equal(t, f.auctions.relists[0].newID, f.auctions.relists[1].newID,
			"the deterministic relist id makes the repeat a no-op on the auction side")
		assert.Equal(t, settlement.StateRelisted, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("seam: crash after both relist effects before the commit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{noRunnerUp: true})
		f.closeSold(t, e)

		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		f.repo.updateErr = nil
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Equal(t, settlement.StateRelisted, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("seam: crash after AwardToRunnerUp before the commit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)

		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		require.Len(t, f.auctions.awards, 1)

		// Redelivery: AwardToRunnerUp again → ErrWinnerAlreadyReassigned
		// → nil on the auction side → commit AwardingRunnerUp.
		f.repo.updateErr = nil
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Len(t, f.auctions.awards, 2)
		assert.Equal(t, settlement.StateAwardingRunnerUp, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("fast-forward: expiry at Started runs the same fork", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnAuctionClosed(ctx, e))
		f.repo.updateErr = nil

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Equal(t, settlement.StateAwardingRunnerUp, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("duplicate expiry past the fork is acked without effects", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{noRunnerUp: true})
		f.closeSold(t, e)
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))
		assert.Len(t, f.auctions.markFails, 1)
		assert.Len(t, f.auctions.relists, 1)
	})
}

func TestOnWinnerReassigned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("issues the second-chance invoice to the runner-up", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		require.NoError(t, f.handlers.OnWinnerReassigned(ctx, e.AuctionID, e.RunnerUpID, e.RunnerUpMinor, e.Currency))

		saga := f.repo.mustGet(t, auctionIDOf(t, e))
		assert.Equal(t, settlement.StateSecondChancePayment, saga.State())
		assert.Equal(t, 2, saga.Attempt())
		assert.Equal(t, secondInvoiceOf(t, e), saga.InvoiceID())
		assert.Equal(t, e.RunnerUpID, saga.Winner().UUID(), "the runner-up is the debtor now")

		require.Len(t, f.billing.issues, 2) // attempt 1 + attempt 2
		second := f.billing.issues[1]
		assert.Equal(t, secondInvoiceOf(t, e), second.invoiceID)
		assert.Equal(t, e.RunnerUpID, second.debtor.UUID())
		assert.Equal(t, e.RunnerUpMinor, second.amount.Amount())
		assert.Equal(t, 2, second.attempt)
	})

	t.Run("fast-forward: the event at AwaitingPayment proves the award", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)

		// The AwardingRunnerUp commit was lost — the saga still says
		// AwaitingPayment when WinnerReassignedV1 arrives (§6.3).
		require.NoError(t, f.handlers.OnWinnerReassigned(ctx, e.AuctionID, e.RunnerUpID, e.RunnerUpMinor, e.Currency))
		saga := f.repo.mustGet(t, auctionIDOf(t, e))
		assert.Equal(t, settlement.StateSecondChancePayment, saga.State())
		assert.Equal(t, 2, saga.Attempt())
	})

	t.Run("seam: crash after IssueInvoice(2) before the commit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		f.repo.updateErr = errors.New("crashed before commit")
		require.Error(t, f.handlers.OnWinnerReassigned(ctx, e.AuctionID, e.RunnerUpID, e.RunnerUpMinor, e.Currency))

		f.repo.updateErr = nil
		require.NoError(t, f.handlers.OnWinnerReassigned(ctx, e.AuctionID, e.RunnerUpID, e.RunnerUpMinor, e.Currency))
		assert.Equal(t, settlement.StateSecondChancePayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("duplicate at SecondChancePayment is acked without effects", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)
		f.awardRunnerUp(t, e)
		issued := len(f.billing.issues)

		require.NoError(t, f.handlers.OnWinnerReassigned(ctx, e.AuctionID, e.RunnerUpID, e.RunnerUpMinor, e.Currency))
		assert.Len(t, f.billing.issues, issued)
	})

	t.Run("a mismatching winner is an anomaly, not an ack", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)
		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, firstInvoiceOf(t, e).UUID()))

		stranger := soldAuction(soldOpts{}).WinnerID
		err := f.handlers.OnWinnerReassigned(ctx, e.AuctionID, stranger, e.RunnerUpMinor, e.Currency)
		assert.ErrorIs(t, err, settlement.ErrWinnerMismatch)
		assert.Len(t, f.billing.issues, 1, "no second-chance invoice for a mismatching event")
	})
}

// TestSecondChanceConclusion drives the saga through the full second
// chance and both of its endings (§6.2).
func TestSecondChanceConclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("paid second chance settles", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)
		f.awardRunnerUp(t, e)

		require.NoError(t, f.handlers.OnInvoicePaid(ctx, e.AuctionID, secondInvoiceOf(t, e).UUID()))
		assert.Equal(t, settlement.StateSettled, f.repo.mustGet(t, auctionIDOf(t, e)).State())
	})

	t.Run("expired second chance relists with the timeout reason", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true})
		f.closeSold(t, e)
		f.awardRunnerUp(t, e)

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, secondInvoiceOf(t, e).UUID()))
		saga := f.repo.mustGet(t, auctionIDOf(t, e))
		assert.Equal(t, settlement.StateRelisted, saga.State())
		assert.Equal(t, settlement.ReasonPaymentTimeout, saga.FailureReason())
		assert.Empty(t, f.auctions.awards[1:], "never a second award")
	})

	t.Run("expired second chance of a relisted generation fails unsold", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		e := soldAuction(soldOpts{qualifies: true, relistGen: 1})
		f.closeSold(t, e)
		f.awardRunnerUp(t, e)

		require.NoError(t, f.handlers.OnInvoiceExpired(ctx, e.AuctionID, secondInvoiceOf(t, e).UUID()))
		saga := f.repo.mustGet(t, auctionIDOf(t, e))
		assert.Equal(t, settlement.StateFailedUnsold, saga.State())
		assert.Empty(t, f.auctions.relists)
	})
}
