package settlement

import "errors"

// State is the closed lifecycle enum of the settlement saga (§2.4,
// state diagram §6.2). Settled, Relisted and FailedUnsold are terminal.
type State struct{ s string }

var (
	StateStarted             = State{"started"}
	StateAwaitingPayment     = State{"awaiting_payment"}
	StateAwardingRunnerUp    = State{"awarding_runner_up"}
	StateSecondChancePayment = State{"second_chance_payment"}
	StateSettled             = State{"settled"}
	StateRelisted            = State{"relisted"}
	StateFailedUnsold        = State{"failed_unsold"}
)

func NewStateFromString(s string) (State, error) {
	switch s {
	case StateStarted.s, StateAwaitingPayment.s, StateAwardingRunnerUp.s,
		StateSecondChancePayment.s, StateSettled.s, StateRelisted.s, StateFailedUnsold.s:
		return State{s: s}, nil
	default:
		return State{}, errors.New("unknown settlement state: " + s)
	}
}

func (s State) String() string { return s.s }
func (s State) IsZero() bool   { return s == State{} }

// IsTerminal reports whether the saga has reached its end. The default
// branch panics: a new state must be classified consciously (rule 12).
func (s State) IsTerminal() bool {
	switch s {
	case StateSettled, StateRelisted, StateFailedUnsold:
		return true
	case StateStarted, StateAwaitingPayment, StateAwardingRunnerUp, StateSecondChancePayment:
		return false
	default:
		panic("settlement: unknown state " + s.s)
	}
}
