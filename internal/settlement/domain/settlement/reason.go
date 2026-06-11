package settlement

import "fmt"

// FailureReason is why a settlement concluded without payment — a
// closed enum (rule 12); zero until the saga reaches Relisted or
// FailedUnsold.
type FailureReason struct{ s string }

var (
	ReasonPaymentTimeout       = FailureReason{"payment_timeout"}
	ReasonSecondChanceDeclined = FailureReason{"second_chance_declined"}
)

func NewFailureReasonFromString(s string) (FailureReason, error) {
	switch s {
	case ReasonPaymentTimeout.s, ReasonSecondChanceDeclined.s:
		return FailureReason{s: s}, nil
	default:
		return FailureReason{}, fmt.Errorf("unknown failure reason: %q", s)
	}
}

func (r FailureReason) String() string { return r.s }
func (r FailureReason) IsZero() bool   { return r == FailureReason{} }
