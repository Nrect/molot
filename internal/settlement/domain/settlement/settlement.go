// Package settlement is the settlement context domain: the Settlement
// process-manager aggregate — the saga state machine of ARCHITECTURE.md
// §2.4/§6 — with its value objects and the repository contract.
//
// The saga step protocol is decide → effect → commit (§6.1):
//
//   - decide-phase functions are PURE (value receiver, no mutation) —
//     they answer "what should happen from the current state" or report
//     ErrUnexpectedTransition for an at-least-once duplicate;
//   - commit-phase methods (pointer receiver) guard and perform the
//     transition atomically; an invalid transition is again
//     ErrUnexpectedTransition, which callers ack — the effects between
//     the phases were idempotent.
//
// The package depends on nothing above it; github.com/google/uuid is
// the single allowed identity primitive (§2.1).
package settlement

import "errors"

// Settlement is the state of the post-hammer settlement saga for one
// auction. All fields are unexported (rule 8); the full transition
// table is §6.3.
type Settlement struct {
	auctionID         AuctionID
	state             State
	winner            BidderID // current debtor: the auction winner, then the runner-up after award
	hammer            Money    // the original hammer price
	runnerUp          BidderID // zero = no runner-up
	runnerUpAmount    Money
	runnerUpQualifies bool
	relistGen         int // 0 = original auction, 1 = already a relist (cap, §6.2)
	attempt           int // 1 = winner's invoice, 2 = second chance
	invoiceID         InvoiceID
	failureReason     FailureReason // zero until Relisted / FailedUnsold
	version           int64         // optimistic-lock mapping; the adapter increments
}

// Start begins the saga from a sold closing (AuctionClosedV1,
// outcome=sold): state Started, attempt 1, the deterministic attempt-1
// invoice id recorded from birth (see Closing).
func Start(c Closing) (*Settlement, error) {
	if c.IsZero() {
		return nil, errors.New("closing is required")
	}
	return &Settlement{
		auctionID:         c.auctionID,
		state:             StateStarted,
		winner:            c.winner,
		hammer:            c.hammer,
		runnerUp:          c.runnerUp,
		runnerUpAmount:    c.runnerUpAmount,
		runnerUpQualifies: c.runnerUpQualifies,
		relistGen:         c.relistGen,
		attempt:           1,
		invoiceID:         c.firstInvoice,
		version:           1,
	}, nil
}

// UnmarshalFromDatabase rehydrates the saga state from storage — the
// only way an adapter may construct the aggregate (rule 7).
func UnmarshalFromDatabase(
	auctionID AuctionID,
	state State,
	winner BidderID,
	hammer Money,
	runnerUp BidderID,
	runnerUpAmount Money,
	runnerUpQualifies bool,
	relistGen int,
	attempt int,
	invoiceID InvoiceID,
	failureReason FailureReason,
	version int64,
) (*Settlement, error) {
	switch {
	case auctionID.IsZero():
		return nil, errors.New("auction id is required")
	case state.IsZero():
		return nil, errors.New("state is required")
	case winner.IsZero():
		return nil, errors.New("winner is required")
	case hammer.IsZero():
		return nil, errors.New("hammer price is required")
	case attempt != 1 && attempt != 2:
		return nil, errors.New("attempt must be 1 or 2")
	case version < 1:
		return nil, errors.New("version must be at least 1")
	}
	return &Settlement{
		auctionID:         auctionID,
		state:             state,
		winner:            winner,
		hammer:            hammer,
		runnerUp:          runnerUp,
		runnerUpAmount:    runnerUpAmount,
		runnerUpQualifies: runnerUpQualifies,
		relistGen:         relistGen,
		attempt:           attempt,
		invoiceID:         invoiceID,
		failureReason:     failureReason,
		version:           version,
	}, nil
}

// --- decide phase: pure decision functions (value receivers) ---------------

// DecideOnInvoiceIssue answers whether the attempt-1 IssueInvoice
// effect is still due. Past Started the AuctionClosedV1 delivery is a
// duplicate → ErrUnexpectedTransition (ack).
func (s Settlement) DecideOnInvoiceIssue() (bool, error) {
	if s.state == StateStarted {
		return true, nil
	}
	return false, ErrUnexpectedTransition
}

// DecideOnPaymentReceived guards InvoicePaidV1 before the
// ConfirmSettlement effect: the paid invoice must be the one the saga
// currently awaits.
func (s Settlement) DecideOnPaymentReceived(inv InvoiceID) error {
	return s.guardLiveInvoice(inv)
}

// DecideOnPaymentTimeout resolves InvoiceExpiredV1 into the
// compensation fork (§6.2): award the runner-up on the first attempt,
// otherwise relist once, then give up.
func (s Settlement) DecideOnPaymentTimeout(inv InvoiceID) (NextStep, error) {
	if err := s.guardLiveInvoice(inv); err != nil {
		return 0, err
	}
	return s.nextStepOnFailure(), nil
}

// guardLiveInvoice admits invoice events only in the states that await
// a payment and only for the invoice the saga currently tracks.
// StateStarted is the fast-forward seam of §6.3: an event for the
// deterministic attempt-1 invoice proves IssueInvoice already happened
// even though the AwaitingPayment commit was lost to a crash. A stale
// invoice of a past attempt (or a foreign one) is an at-least-once
// duplicate → ErrUnexpectedTransition.
func (s Settlement) guardLiveInvoice(inv InvoiceID) error {
	switch s.state {
	case StateStarted, StateAwaitingPayment, StateSecondChancePayment:
		if inv.IsZero() || inv != s.invoiceID {
			return ErrUnexpectedTransition
		}
		return nil
	default:
		return ErrUnexpectedTransition
	}
}

// DecideOnWinnerReassigned guards WinnerReassignedV1 before the
// attempt-2 IssueInvoice effect. StateAwaitingPayment is the
// fast-forward seam (§6.3): the event proves AwardToRunnerUp committed
// before our own AwardingRunnerUp commit landed. A winner or price that
// does not match the recorded runner-up is a cross-context anomaly —
// ErrWinnerMismatch, never acked.
func (s Settlement) DecideOnWinnerReassigned(newWinner BidderID, price Money) error {
	switch s.state {
	case StateAwaitingPayment, StateAwardingRunnerUp:
		if newWinner.IsZero() || newWinner != s.runnerUp || price != s.runnerUpAmount {
			return ErrWinnerMismatch
		}
		return nil
	default:
		return ErrUnexpectedTransition
	}
}

// CanRunnerUpDecline is the pure authorization rule of
// DeclineSecondChanceOffer (rule 22, §6.4): only the runner-up holding
// the live second-chance offer may decline it.
func CanRunnerUpDecline(actor BidderID, s Settlement) error {
	if actor.IsZero() || s.runnerUp.IsZero() || actor != s.runnerUp || s.state != StateSecondChancePayment {
		return ErrNotOfferRecipient
	}
	return nil
}

// DecideOnDecline resolves the runner-up's decline into the same
// compensation fork as a payment timeout (§6.4). For the genuine offer
// recipient retrying after the saga has moved on, the verdict is
// refined: a matching outcome (Relisted/FailedUnsold) is
// ErrUnexpectedTransition — the caller's idempotent 204; a paid offer
// is ErrOfferAlreadyPaid — the payment won.
func (s Settlement) DecideOnDecline(actor BidderID) (NextStep, error) {
	if err := CanRunnerUpDecline(actor, s); err != nil {
		if !actor.IsZero() && actor == s.runnerUp {
			switch s.state {
			case StateSettled:
				return 0, ErrOfferAlreadyPaid
			case StateRelisted, StateFailedUnsold:
				return 0, ErrUnexpectedTransition
			}
		}
		return 0, err
	}
	return s.nextStepOnFailure(), nil
}

// nextStepOnFailure is the compensation decision (§6.2): a qualifying
// runner-up gets the second chance on the first attempt; otherwise the
// auction is relisted once (relistGen 0) and then fails for good.
func (s Settlement) nextStepOnFailure() NextStep {
	if s.attempt == 1 && s.runnerUpQualifies {
		return StepAwardRunnerUp
	}
	if s.relistGen == 0 {
		return StepRelist
	}
	return StepFailUnsold
}

// --- commit phase: guarded transitions (pointer receivers) -----------------

// InvoiceIssued commits Started → AwaitingPayment after the attempt-1
// IssueInvoice effect.
func (s *Settlement) InvoiceIssued(inv InvoiceID) error {
	if s.state != StateStarted || inv.IsZero() || inv != s.invoiceID {
		return ErrUnexpectedTransition
	}
	s.state = StateAwaitingPayment
	return nil
}

// PaymentReceived commits the saga to Settled after the
// ConfirmSettlement effect. Accepted from AwaitingPayment,
// SecondChancePayment and the Started fast-forward seam (§6.3) — always
// for the currently awaited invoice only.
func (s *Settlement) PaymentReceived(inv InvoiceID) error {
	if err := s.guardLiveInvoice(inv); err != nil {
		return err
	}
	s.state = StateSettled
	return nil
}

// RunnerUpAwarded fixes the AwardingRunnerUp transition when
// WinnerReassignedV1 arrives: the runner-up becomes the debtor of
// attempt 2. From AwaitingPayment it is the fast-forward seam (§6.3);
// from AwardingRunnerUp it confirms the state set by ApplyNextStep.
func (s *Settlement) RunnerUpAwarded(newWinner BidderID, price Money) error {
	if err := s.DecideOnWinnerReassigned(newWinner, price); err != nil {
		return err
	}
	s.state = StateAwardingRunnerUp
	s.winner = newWinner
	s.attempt = 2
	return nil
}

// SecondChanceInvoiceIssued commits AwardingRunnerUp →
// SecondChancePayment after the attempt-2 IssueInvoice effect; the saga
// now awaits the second-chance invoice.
func (s *Settlement) SecondChanceInvoiceIssued(inv InvoiceID) error {
	if s.state != StateAwardingRunnerUp || inv.IsZero() {
		return ErrUnexpectedTransition
	}
	s.state = StateSecondChancePayment
	s.invoiceID = inv
	return nil
}

// ApplyNextStep commits the compensation fork decided by
// DecideOnPaymentTimeout / DecideOnDecline:
//
//   - StepAwardRunnerUp → AwardingRunnerUp (no failure reason — the sale
//     may yet settle);
//   - StepRelist → Relisted (terminal) with the failure reason;
//   - StepFailUnsold → FailedUnsold (terminal) with the failure reason.
//
// StateStarted is accepted alongside the awaiting states for the
// fast-forward seam of §6.3. An unknown step panics: closed enum
// (rule 12).
func (s *Settlement) ApplyNextStep(step NextStep, reason FailureReason) error {
	switch step {
	case StepAwardRunnerUp:
		if !reason.IsZero() {
			return errors.New("awarding the runner-up carries no failure reason")
		}
		if s.state != StateStarted && s.state != StateAwaitingPayment {
			return ErrUnexpectedTransition
		}
		s.state = StateAwardingRunnerUp
		return nil
	case StepRelist, StepFailUnsold:
		if reason.IsZero() {
			return errors.New("a failed settlement requires a failure reason")
		}
		switch s.state {
		case StateStarted, StateAwaitingPayment, StateSecondChancePayment:
			if step == StepRelist {
				s.state = StateRelisted
			} else {
				s.state = StateFailedUnsold
			}
			s.failureReason = reason
			return nil
		default:
			return ErrUnexpectedTransition
		}
	default:
		panic("settlement: unknown next step")
	}
}

// --- predicates and typed getters (mapping; no setters, no GetX) -----------

func (s Settlement) AuctionID() AuctionID         { return s.auctionID }
func (s Settlement) State() State                 { return s.state }
func (s Settlement) Winner() BidderID             { return s.winner }
func (s Settlement) Hammer() Money                { return s.hammer }
func (s Settlement) RunnerUp() BidderID           { return s.runnerUp }
func (s Settlement) RunnerUpAmount() Money        { return s.runnerUpAmount }
func (s Settlement) RunnerUpQualifies() bool      { return s.runnerUpQualifies }
func (s Settlement) RelistGeneration() int        { return s.relistGen }
func (s Settlement) Attempt() int                 { return s.attempt }
func (s Settlement) InvoiceID() InvoiceID         { return s.invoiceID }
func (s Settlement) FailureReason() FailureReason { return s.failureReason }
func (s Settlement) Version() int64               { return s.version }
