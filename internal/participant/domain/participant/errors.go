package participant

import (
	"errors"
	"fmt"
)

// Sentinel errors — invariant violations exported package-level (BOOK_AUDIT
// rule 10). Callers never re-check what the domain already guarantees.
var (
	ErrInvalidParticipantID = errors.New("participant id must be a non-zero uuid")
	ErrInvalidEmail         = errors.New("email address is invalid")
	ErrInvalidStatus        = errors.New("participant status is invalid")
	ErrEmptyDisplayName     = errors.New("display name must not be empty")
	ErrAlreadyVerified      = errors.New("participant is already verified")
	ErrEmailTaken           = errors.New("email address is already taken")
)

// NotFoundError reports a missing participant; adapters map driver
// not-found (sql.ErrNoRows) into it so driver errors never leak above
// the repository (rule 19).
type NotFoundError struct {
	ID ParticipantID
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("participant %s not found", e.ID)
}

// ForbiddenParticipantUpdateError reports an actor touching a profile
// that is not their own. The repository raises it from the
// CanActorUpdateParticipant guard inside the update transaction.
type ForbiddenParticipantUpdateError struct {
	Actor  ParticipantID
	Target ParticipantID
}

func (e ForbiddenParticipantUpdateError) Error() string {
	return fmt.Sprintf("actor %s may not update participant %s", e.Actor, e.Target)
}
