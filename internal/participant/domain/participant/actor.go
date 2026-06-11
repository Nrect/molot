package participant

// Actor is the acting participant of a self-service mutation. Repos take
// it as an explicit typed parameter — identity is never read from
// context.Context inside a repository (BOOK_AUDIT rule 21).
type Actor struct {
	id ParticipantID
}

func NewActor(id ParticipantID) (Actor, error) {
	if id.IsZero() {
		return Actor{}, ErrInvalidParticipantID
	}
	return Actor{id: id}, nil
}

func (a Actor) ParticipantID() ParticipantID { return a.id }

func (a Actor) IsZero() bool { return a == Actor{} }

// CanActorUpdateParticipant is the authorization rule for self-service
// profile updates as a pure domain function (rule 22): a participant may
// update only their own profile. The repository calls it inside the
// update transaction, before updateFn.
func CanActorUpdateParticipant(actor Actor, p Participant) error {
	if actor.id != p.id {
		return ForbiddenParticipantUpdateError{Actor: actor.id, Target: p.id}
	}
	return nil
}
