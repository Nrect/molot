// Package command holds the settlement context's single write use case
// (DeclineSecondChanceOffer, §6.4) and the deterministic saga
// identities shared with the event handlers.
package command

import (
	"context"
	"errors"
	"time"

	"molot/internal/common/errs"
	"molot/internal/settlement/domain/settlement"
)

// clock is the consumer-side time source (rule 5): the relist window is
// computed from it, keeping tests deterministic.
type clock interface {
	Now() time.Time
}

// Consumer-side gateways (rule 5, §6): exactly the slice of the foreign
// facades THIS use case needs. Adapters bridge them to the auction and
// billing service facades; every call is idempotent on their side.
type auctionGateway interface {
	MarkSaleFailed(ctx context.Context, auctionID settlement.AuctionID, reason settlement.FailureReason) error
	Relist(ctx context.Context, originalID, newID settlement.AuctionID, startsAt, endsAt time.Time) error
}

type billingGateway interface {
	// VoidInvoice cancels the pending second-chance invoice. Contract:
	// already voided/expired → nil; already PAID →
	// settlement.ErrOfferAlreadyPaid (the adapter translates billing's
	// conflict) — the payment won and the decline must fail (§6.4).
	VoidInvoice(ctx context.Context, invoiceID settlement.InvoiceID) error
}

// DeclineSecondChanceOffer: the runner-up declines the offer without
// waiting for the payment term. It runs the saga step protocol
// decide → effect → commit (§6.1) and lands in the same domain fork as
// a payment timeout, with reason=second_chance_declined.
type DeclineSecondChanceOffer struct {
	AuctionID settlement.AuctionID
	Actor     settlement.BidderID
}

type DeclineSecondChanceOfferHandler struct {
	repo     settlement.Repository
	auctions auctionGateway
	billing  billingGateway
	relist   settlement.RelistPolicy
	clock    clock
}

func NewDeclineSecondChanceOfferHandler(
	repo settlement.Repository,
	auctions auctionGateway,
	billing billingGateway,
	relist settlement.RelistPolicy,
	clk clock,
) DeclineSecondChanceOfferHandler {
	if repo == nil {
		panic("NewDeclineSecondChanceOfferHandler: nil repo")
	}
	if auctions == nil {
		panic("NewDeclineSecondChanceOfferHandler: nil auction gateway")
	}
	if billing == nil {
		panic("NewDeclineSecondChanceOfferHandler: nil billing gateway")
	}
	if relist.IsZero() {
		panic("NewDeclineSecondChanceOfferHandler: zero relist policy")
	}
	if clk == nil {
		panic("NewDeclineSecondChanceOfferHandler: nil clock")
	}
	return DeclineSecondChanceOfferHandler{
		repo: repo, auctions: auctions, billing: billing, relist: relist, clock: clk,
	}
}

func (h DeclineSecondChanceOfferHandler) Handle(ctx context.Context, cmd DeclineSecondChanceOffer) error {
	// decide: a short read plus the pure domain verdict (authorization
	// included — CanRunnerUpDecline inside DecideOnDecline).
	s, err := h.repo.Get(ctx, cmd.AuctionID)
	if err != nil {
		return mapNotFound(err)
	}
	step, err := s.DecideOnDecline(cmd.Actor)
	if err != nil {
		return declineOutcome(err)
	}

	// effect 1 — strictly outside any settlement transaction (§6.1):
	// void the live second-chance invoice. A paid invoice wins the race
	// and surfaces as 409 offer-already-paid; InvoicePaidV1 will finish
	// the saga (§6.4).
	if err := h.billing.VoidInvoice(ctx, s.InvoiceID()); err != nil {
		if errors.Is(err, settlement.ErrOfferAlreadyPaid) {
			return errs.NewConflictError("offer-already-paid").WithCause(err)
		}
		return err
	}

	// effect 2: the compensation fork, same as a payment timeout but
	// with the decline reason. Both effects are idempotent — an HTTP
	// retry after a crash between them simply repeats no-ops.
	if err := h.compensate(ctx, cmd.AuctionID, step); err != nil {
		return err
	}

	// commit: the state transition under FOR UPDATE + version. Losing
	// the benign race against the expiry worker yields
	// ErrUnexpectedTransition — re-read and answer by the final state
	// (§6.4): the same outcome is the idempotent 204.
	err = h.repo.Update(ctx, cmd.AuctionID,
		func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
			if err := current.ApplyNextStep(step, settlement.ReasonSecondChanceDeclined); err != nil {
				return nil, err
			}
			return current, nil
		})
	if errors.Is(err, settlement.ErrUnexpectedTransition) {
		current, getErr := h.repo.Get(ctx, cmd.AuctionID)
		if getErr != nil {
			return mapNotFound(getErr)
		}
		_, decideErr := current.DecideOnDecline(cmd.Actor)
		return declineOutcome(decideErr)
	}
	return err
}

// compensate executes the effect phase of the decided fork. Award is
// impossible on attempt 2 — the closed-enum default panics (rule 12).
func (h DeclineSecondChanceOfferHandler) compensate(
	ctx context.Context,
	auctionID settlement.AuctionID,
	step settlement.NextStep,
) error {
	switch step {
	case settlement.StepRelist:
		if err := h.auctions.MarkSaleFailed(ctx, auctionID, settlement.ReasonSecondChanceDeclined); err != nil {
			return err
		}
		startsAt, endsAt := h.relist.Window(h.clock.Now())
		return h.auctions.Relist(ctx, auctionID, RelistIDFor(auctionID), startsAt, endsAt)
	case settlement.StepFailUnsold:
		return h.auctions.MarkSaleFailed(ctx, auctionID, settlement.ReasonSecondChanceDeclined)
	default:
		panic("settlement decline: unexpected next step " + step.String())
	}
}

// declineOutcome maps the domain verdict to the slug contract of §6.4.
// ErrUnexpectedTransition means the saga already reached the same
// outcome — the idempotent success (204).
func declineOutcome(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, settlement.ErrUnexpectedTransition):
		return nil
	case errors.Is(err, settlement.ErrOfferAlreadyPaid):
		return errs.NewConflictError("offer-already-paid").WithCause(err)
	case errors.Is(err, settlement.ErrNotOfferRecipient):
		return errs.NewForbiddenError("not-offer-recipient").WithCause(err)
	default:
		return err
	}
}

func mapNotFound(err error) error {
	var notFound settlement.NotFoundError
	if errors.As(err, &notFound) {
		return errs.NewNotFoundError("settlement-not-found").WithCause(err)
	}
	return err
}
