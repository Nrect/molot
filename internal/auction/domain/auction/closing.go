package auction

import "fmt"

// ClosingResult is what Close hands back to the caller (the closing
// worker → outbox → settlement saga). RunnerUpQualifies is the
// ready-made second-chance verdict, so the reserve price itself never
// leaves the aggregate (§2.1).
type ClosingResult struct {
	Outcome           Outcome
	Winner            Bid // zero unless Outcome is sold
	RunnerUp          Bid // zero when there was at most one bidder
	RunnerUpQualifies bool
}

// FailureReason is why a settlement failed — a closed enum (rule 12).
type FailureReason struct {
	s string
}

var (
	ReasonPaymentTimeout       = FailureReason{"payment_timeout"}
	ReasonSecondChanceDeclined = FailureReason{"second_chance_declined"}
)

func NewFailureReasonFromString(s string) (FailureReason, error) {
	switch s {
	case ReasonPaymentTimeout.s, ReasonSecondChanceDeclined.s:
		return FailureReason{s}, nil
	}
	return FailureReason{}, fmt.Errorf("%w: %q", ErrInvalidFailureReason, s)
}

func (r FailureReason) IsZero() bool   { return r == FailureReason{} }
func (r FailureReason) String() string { return r.s }
