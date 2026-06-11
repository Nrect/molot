// Package app is the application layer of the settlement context: the
// use-case catalog (app.go) and the saga's typed event handlers — the
// orchestration heart of ARCHITECTURE.md §6.
//
// Every handler follows the step protocol decide → effect → commit
// (§6.1): a short read plus a pure domain decision, then idempotent
// synchronous gateway calls STRICTLY outside any settlement
// transaction, then the guarded state transition under FOR UPDATE.
// ErrUnexpectedTransition at decide or commit means an at-least-once
// duplicate (or a lost benign race) — the handler acks it; any crash
// between the phases is healed by redelivery (crash-seam map §6.7).
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"molot/internal/settlement/app/command"
	"molot/internal/settlement/domain/settlement"
)

type clock interface {
	Now() time.Time
}

// Consumer-side gateways (rule 5, §6, ADR-0004): the slice of the
// foreign sync facades the saga's event handlers need. Adapters bridge
// them to auctionservice.Facade / billingservice.Facade; every command
// is idempotent on the far side ("already in the target state" → nil),
// which makes the effect phase safely repeatable.
type auctionGateway interface {
	AwardToRunnerUp(ctx context.Context, auctionID settlement.AuctionID) error
	Relist(ctx context.Context, originalID, newID settlement.AuctionID, startsAt, endsAt time.Time) error
	MarkSaleFailed(ctx context.Context, auctionID settlement.AuctionID, reason settlement.FailureReason) error
	ConfirmSettlement(ctx context.Context, auctionID settlement.AuctionID) error
}

type billingGateway interface {
	IssueInvoice(ctx context.Context, invoiceID settlement.InvoiceID, auctionID settlement.AuctionID,
		debtor settlement.BidderID, amount settlement.Money, attempt int) error
}

// EventHandlers is the saga reacting to the integration events of
// auction and billing (subscription matrix §4.3). Ports parse the raw
// payloads and call these methods; cross-cutting observability comes
// from the watermill middleware, not decorators.
type EventHandlers struct {
	repo     settlement.Repository
	auctions auctionGateway
	billing  billingGateway
	relist   settlement.RelistPolicy
	clock    clock
}

func NewEventHandlers(
	repo settlement.Repository,
	auctions auctionGateway,
	billing billingGateway,
	relist settlement.RelistPolicy,
	clk clock,
) EventHandlers {
	if repo == nil {
		panic("NewEventHandlers: nil repo")
	}
	if auctions == nil {
		panic("NewEventHandlers: nil auction gateway")
	}
	if billing == nil {
		panic("NewEventHandlers: nil billing gateway")
	}
	if relist.IsZero() {
		panic("NewEventHandlers: zero relist policy")
	}
	if clk == nil {
		panic("NewEventHandlers: nil clock")
	}
	return EventHandlers{repo: repo, auctions: auctions, billing: billing, relist: relist, clock: clk}
}

// ClosedAuction is the parsed payload of AuctionClosedV1 the port hands
// in. WinnerID/RunnerUpID are uuid.Nil when absent.
type ClosedAuction struct {
	AuctionID         uuid.UUID
	Outcome           string
	WinnerID          uuid.UUID
	HammerMinor       int64
	Currency          string
	RunnerUpID        uuid.UUID
	RunnerUpMinor     int64
	RunnerUpQualifies bool
	RelistGeneration  int
}

// OnAuctionClosed starts the saga (§6.3 row 1 and the Started
// continuation row): INSERT ON CONFLICT DO NOTHING, re-load, then issue
// the attempt-1 invoice and commit AwaitingPayment.
func (h EventHandlers) OnAuctionClosed(ctx context.Context, e ClosedAuction) error {
	outcome, err := settlement.NewOutcomeFromString(e.Outcome)
	if err != nil {
		return fmt.Errorf("auction closed event: %w", err) // poison → dead letter (§6.8)
	}
	if !outcome.RequiresSettlement() {
		return nil // not_sold closes without a saga
	}

	closing, err := h.buildClosing(e)
	if err != nil {
		return fmt.Errorf("auction closed event: %w", err)
	}

	// decide: idempotent insert (PK auction_id), then the authoritative
	// stored state decides — a crash after the insert is continued here.
	started, err := settlement.Start(closing)
	if err != nil {
		return err
	}
	if err := h.repo.Add(ctx, started); err != nil {
		return err
	}
	current, err := h.repo.Get(ctx, closing.AuctionID())
	if err != nil {
		return err
	}
	needIssue, err := current.DecideOnInvoiceIssue()
	if err != nil {
		return ackUnexpected(err) // past Started: a duplicate delivery
	}
	if !needIssue {
		return nil
	}

	// effect: idempotent on the billing side — the deterministic id and
	// UNIQUE(auction_id, attempt) turn a repeat into a no-op.
	if err := h.billing.IssueInvoice(ctx,
		current.InvoiceID(), current.AuctionID(), current.Winner(), current.Hammer(), current.Attempt(),
	); err != nil {
		return err
	}

	// commit.
	return ackUnexpected(h.repo.Update(ctx, closing.AuctionID(),
		func(_ context.Context, s *settlement.Settlement) (*settlement.Settlement, error) {
			if err := s.InvoiceIssued(current.InvoiceID()); err != nil {
				return nil, err
			}
			return s, nil
		}))
}

// OnInvoicePaid settles the saga (§6.3): ConfirmSettlement on the
// auction, then the Settled commit. Started with the matching
// deterministic attempt-1 invoice fast-forwards (§6.3 special seam).
func (h EventHandlers) OnInvoicePaid(ctx context.Context, auctionID, invoiceID uuid.UUID) error {
	aucID, invID, err := parseSagaIDs(auctionID, invoiceID)
	if err != nil {
		return err
	}
	s, err := h.repo.Get(ctx, aucID)
	if err != nil {
		return err // an invoice the saga never issued — an anomaly, not an ack
	}
	if err := s.DecideOnPaymentReceived(invID); err != nil {
		return ackUnexpected(err)
	}

	if err := h.auctions.ConfirmSettlement(ctx, aucID); err != nil {
		return err
	}

	return ackUnexpected(h.repo.Update(ctx, aucID,
		func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
			if err := current.PaymentReceived(invID); err != nil {
				return nil, err
			}
			return current, nil
		}))
}

// OnInvoiceExpired runs the compensation fork (§6.2/§6.3): second
// chance, relist or final failure — decided by the domain.
func (h EventHandlers) OnInvoiceExpired(ctx context.Context, auctionID, invoiceID uuid.UUID) error {
	aucID, invID, err := parseSagaIDs(auctionID, invoiceID)
	if err != nil {
		return err
	}
	s, err := h.repo.Get(ctx, aucID)
	if err != nil {
		return err
	}
	step, err := s.DecideOnPaymentTimeout(invID)
	if err != nil {
		return ackUnexpected(err) // stale attempt or already concluded
	}

	if err := h.compensate(ctx, aucID, step, settlement.ReasonPaymentTimeout); err != nil {
		return err
	}

	var commitReason settlement.FailureReason
	if step != settlement.StepAwardRunnerUp {
		commitReason = settlement.ReasonPaymentTimeout
	}
	return ackUnexpected(h.repo.Update(ctx, aucID,
		func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
			if err := current.ApplyNextStep(step, commitReason); err != nil {
				return nil, err
			}
			return current, nil
		}))
}

// OnWinnerReassigned issues the second-chance invoice (§6.3). The
// AwaitingPayment fast-forward row is honored: the event itself proves
// AwardToRunnerUp committed even if our AwardingRunnerUp commit was
// lost. A mismatching winner or price is ErrWinnerMismatch — retried to
// the dead letter, never silently dropped.
func (h EventHandlers) OnWinnerReassigned(
	ctx context.Context,
	auctionID, newWinnerID uuid.UUID,
	priceMinor int64,
	currency string,
) error {
	aucID, err := settlement.NewAuctionID(auctionID)
	if err != nil {
		return fmt.Errorf("winner reassigned event: %w", err)
	}
	newWinner, err := settlement.NewBidderID(newWinnerID)
	if err != nil {
		return fmt.Errorf("winner reassigned event: %w", err)
	}
	cur, err := settlement.NewCurrency(currency)
	if err != nil {
		return fmt.Errorf("winner reassigned event: %w", err)
	}
	price, err := settlement.NewMoney(priceMinor, cur)
	if err != nil {
		return fmt.Errorf("winner reassigned event: %w", err)
	}

	s, err := h.repo.Get(ctx, aucID)
	if err != nil {
		return err
	}
	if err := s.DecideOnWinnerReassigned(newWinner, price); err != nil {
		return ackUnexpected(err)
	}

	// effect: the attempt-2 invoice for the runner-up; the settlement's
	// own record is authoritative for debtor and amount (§6.3).
	secondInvoice := command.InvoiceIDForAttempt(aucID, 2)
	if err := h.billing.IssueInvoice(ctx,
		secondInvoice, aucID, s.RunnerUp(), s.RunnerUpAmount(), 2,
	); err != nil {
		return err
	}

	// commit: both transitions in one transaction — fix the award
	// (fast-forward included) and start awaiting the new invoice.
	return ackUnexpected(h.repo.Update(ctx, aucID,
		func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
			if err := current.RunnerUpAwarded(newWinner, price); err != nil {
				return nil, err
			}
			if err := current.SecondChanceInvoiceIssued(secondInvoice); err != nil {
				return nil, err
			}
			return current, nil
		}))
}

// compensate executes the effect phase of a NextStep fork. A crash
// between the two relist effects is healed by redelivery: both repeat
// as no-ops on the auction side (§6.7).
func (h EventHandlers) compensate(
	ctx context.Context,
	auctionID settlement.AuctionID,
	step settlement.NextStep,
	reason settlement.FailureReason,
) error {
	switch step {
	case settlement.StepAwardRunnerUp:
		return h.auctions.AwardToRunnerUp(ctx, auctionID)
	case settlement.StepRelist:
		if err := h.auctions.MarkSaleFailed(ctx, auctionID, reason); err != nil {
			return err
		}
		startsAt, endsAt := h.relist.Window(h.clock.Now())
		return h.auctions.Relist(ctx, auctionID, command.RelistIDFor(auctionID), startsAt, endsAt)
	case settlement.StepFailUnsold:
		return h.auctions.MarkSaleFailed(ctx, auctionID, reason)
	default:
		panic("settlement: unexpected next step " + step.String())
	}
}

func (h EventHandlers) buildClosing(e ClosedAuction) (settlement.Closing, error) {
	auctionID, err := settlement.NewAuctionID(e.AuctionID)
	if err != nil {
		return settlement.Closing{}, err
	}
	winner, err := settlement.NewBidderID(e.WinnerID)
	if err != nil {
		return settlement.Closing{}, err
	}
	currency, err := settlement.NewCurrency(e.Currency)
	if err != nil {
		return settlement.Closing{}, err
	}
	hammer, err := settlement.NewMoney(e.HammerMinor, currency)
	if err != nil {
		return settlement.Closing{}, err
	}
	var runnerUp settlement.BidderID
	var runnerUpAmount settlement.Money
	if e.RunnerUpID != uuid.Nil {
		if runnerUp, err = settlement.NewBidderID(e.RunnerUpID); err != nil {
			return settlement.Closing{}, err
		}
		if runnerUpAmount, err = settlement.NewMoney(e.RunnerUpMinor, currency); err != nil {
			return settlement.Closing{}, err
		}
	}
	return settlement.NewClosing(
		auctionID, winner, hammer,
		runnerUp, runnerUpAmount, e.RunnerUpQualifies,
		e.RelistGeneration,
		command.InvoiceIDForAttempt(auctionID, 1),
	)
}

func parseSagaIDs(auctionID, invoiceID uuid.UUID) (settlement.AuctionID, settlement.InvoiceID, error) {
	aucID, err := settlement.NewAuctionID(auctionID)
	if err != nil {
		return settlement.AuctionID{}, settlement.InvoiceID{}, fmt.Errorf("invoice event: %w", err)
	}
	invID, err := settlement.NewInvoiceID(invoiceID)
	if err != nil {
		return settlement.AuctionID{}, settlement.InvoiceID{}, fmt.Errorf("invoice event: %w", err)
	}
	return aucID, invID, nil
}

// ackUnexpected turns ErrUnexpectedTransition into an ack (nil): the
// input was an at-least-once duplicate or a lost benign race; the
// effects were idempotent (§6.1).
func ackUnexpected(err error) error {
	if errors.Is(err, settlement.ErrUnexpectedTransition) {
		return nil
	}
	return err
}
