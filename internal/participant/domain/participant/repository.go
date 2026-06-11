package participant

import "context"

// Repository is the minimal persistence port of the aggregate
// (BOOK_AUDIT rules 15–16, 21, 23). Mutations go through updateFn
// closures: the adapter owns the transaction, persists the returned
// aggregate and rolls back when the closure errors.
type Repository interface {
	// Add persists a freshly registered participant. A duplicate —
	// whether the client-generated id or the unique email is already
	// registered — is reported as ErrEmailTaken: both mean this
	// registration was already made and the address is in use.
	Add(ctx context.Context, p *Participant) error

	// Get loads a participant; missing id → NotFoundError. Profiles are
	// readable by any authenticated caller (§8), so no actor parameter.
	Get(ctx context.Context, id ParticipantID) (*Participant, error)

	// Update mutates the participant on behalf of an acting participant.
	// The adapter enforces CanActorUpdateParticipant inside the
	// transaction, before updateFn (rule 22).
	Update(
		ctx context.Context,
		id ParticipantID,
		actor Actor,
		updateFn func(ctx context.Context, p *Participant) (*Participant, error),
	) error

	// UpdateAsOperations is the system path for the operations role
	// (verification). The name screams about the security implication —
	// no ownership guard runs (rule 23); ports gate the role.
	UpdateAsOperations(
		ctx context.Context,
		id ParticipantID,
		updateFn func(ctx context.Context, p *Participant) (*Participant, error),
	) error
}
