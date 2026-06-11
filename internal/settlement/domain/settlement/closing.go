package settlement

import "errors"

// Outcome mirrors the auction closing verdict ("sold" | "not_sold") —
// settlement's own copy of the concept (rule 4); parsed from
// AuctionClosedV1.
type Outcome struct{ s string }

var (
	OutcomeSold    = Outcome{"sold"}
	OutcomeNotSold = Outcome{"not_sold"}
)

func NewOutcomeFromString(s string) (Outcome, error) {
	switch s {
	case OutcomeSold.s, OutcomeNotSold.s:
		return Outcome{s: s}, nil
	default:
		return Outcome{}, errors.New("unknown auction outcome: " + s)
	}
}

func (o Outcome) String() string { return o.s }
func (o Outcome) IsZero() bool   { return o == Outcome{} }

// RequiresSettlement: only a sold hammer starts a saga (§6.2). The
// default branch panics — a new outcome must be classified consciously
// (rule 12).
func (o Outcome) RequiresSettlement() bool {
	switch o {
	case OutcomeSold:
		return true
	case OutcomeNotSold:
		return false
	default:
		panic("settlement: unknown outcome " + o.s)
	}
}

// Closing is the validated input of Start, built from AuctionClosedV1
// (outcome=sold). FirstInvoice is the deterministic attempt-1 invoice
// id derived by the app layer (uuidv5, §6.6): recording it at birth
// lets the state machine verify the fast-forward seams of §6.3 — an
// InvoicePaid/InvoiceExpired for that exact invoice arriving while the
// saga is still Started proves IssueInvoice already happened.
type Closing struct {
	auctionID         AuctionID
	winner            BidderID
	hammer            Money
	runnerUp          BidderID // zero = at most one bidder
	runnerUpAmount    Money    // zero iff runnerUp is zero
	runnerUpQualifies bool
	relistGen         int
	firstInvoice      InvoiceID
}

func NewClosing(
	auctionID AuctionID,
	winner BidderID,
	hammer Money,
	runnerUp BidderID,
	runnerUpAmount Money,
	runnerUpQualifies bool,
	relistGen int,
	firstInvoice InvoiceID,
) (Closing, error) {
	switch {
	case auctionID.IsZero():
		return Closing{}, errors.New("auction id is required")
	case winner.IsZero():
		return Closing{}, errors.New("winner is required")
	case hammer.IsZero():
		return Closing{}, errors.New("hammer price is required")
	case firstInvoice.IsZero():
		return Closing{}, errors.New("first invoice id is required")
	case relistGen < 0:
		return Closing{}, errors.New("relist generation must not be negative")
	case runnerUp.IsZero() && runnerUpQualifies:
		return Closing{}, errors.New("runner-up cannot qualify without a runner-up")
	case runnerUp.IsZero() != runnerUpAmount.IsZero():
		return Closing{}, errors.New("runner-up and runner-up amount must come together")
	}
	return Closing{
		auctionID:         auctionID,
		winner:            winner,
		hammer:            hammer,
		runnerUp:          runnerUp,
		runnerUpAmount:    runnerUpAmount,
		runnerUpQualifies: runnerUpQualifies,
		relistGen:         relistGen,
		firstInvoice:      firstInvoice,
	}, nil
}

func (c Closing) AuctionID() AuctionID { return c.auctionID }
func (c Closing) IsZero() bool         { return c == Closing{} }
