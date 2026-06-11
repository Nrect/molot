package auction

import "fmt"

// Status is the auction lifecycle state — a closed enum (rule 12).
type Status struct {
	s string
}

var (
	StatusListed    = Status{"listed"}
	StatusCancelled = Status{"cancelled"}
	StatusClosed    = Status{"closed"}
)

func NewStatusFromString(s string) (Status, error) {
	switch s {
	case StatusListed.s, StatusCancelled.s, StatusClosed.s:
		return Status{s}, nil
	}
	return Status{}, fmt.Errorf("%w: %q", ErrInvalidStatus, s)
}

func (s Status) IsZero() bool   { return s == Status{} }
func (s Status) String() string { return s.s }

// Outcome is the closing verdict; the zero value means "not closed
// yet". Only set when Status is closed.
type Outcome struct {
	s string
}

var (
	OutcomeSold    = Outcome{"sold"}
	OutcomeNotSold = Outcome{"not_sold"}
)

func NewOutcomeFromString(s string) (Outcome, error) {
	switch s {
	case OutcomeSold.s, OutcomeNotSold.s:
		return Outcome{s}, nil
	}
	return Outcome{}, fmt.Errorf("%w: %q", ErrInvalidOutcome, s)
}

func (o Outcome) IsZero() bool   { return o == Outcome{} }
func (o Outcome) String() string { return o.s }
