package participant

// Status is the closed enum of participant lifecycle states
// (BOOK_AUDIT rule 12): registered → verified.
type Status struct {
	value string
}

var (
	StatusRegistered = Status{"registered"}
	StatusVerified   = Status{"verified"}
)

// NewStatusFromString reconstructs a Status from its raw persisted
// value; unknown values are rejected with ErrInvalidStatus.
func NewStatusFromString(raw string) (Status, error) {
	switch raw {
	case StatusRegistered.value:
		return StatusRegistered, nil
	case StatusVerified.value:
		return StatusVerified, nil
	default:
		return Status{}, ErrInvalidStatus
	}
}

func (s Status) IsZero() bool { return s == Status{} }

// String panics on a value outside the closed enum — reaching the
// default branch is a programming error, not an input error (rule 12).
func (s Status) String() string {
	switch s {
	case StatusRegistered, StatusVerified:
		return s.value
	default:
		panic("participant: Status.String called on invalid status value")
	}
}
