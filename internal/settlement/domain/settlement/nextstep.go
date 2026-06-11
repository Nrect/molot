package settlement

import "fmt"

// NextStep is the saga's compensation fork, decided by the domain
// (§2.4): what to do after a payment failed or the offer was declined.
// A closed enum — the zero value is invalid and switch defaults panic
// (rule 12).
type NextStep int

const (
	// StepAwardRunnerUp: first attempt failed and a qualifying
	// runner-up exists — make the second-chance offer.
	StepAwardRunnerUp NextStep = iota + 1
	// StepRelist: no (more) second chances and the relist cap is not
	// reached — fail the sale and list a replacement auction.
	StepRelist
	// StepFailUnsold: the relist cap is exhausted — fail the sale for
	// good.
	StepFailUnsold
)

func (n NextStep) String() string {
	switch n {
	case StepAwardRunnerUp:
		return "award_runner_up"
	case StepRelist:
		return "relist"
	case StepFailUnsold:
		return "fail_unsold"
	default:
		panic(fmt.Sprintf("settlement: unknown next step %d", int(n)))
	}
}

func (n NextStep) IsZero() bool { return n == 0 }
