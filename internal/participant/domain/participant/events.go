package participant

import "time"

// DomainEvent is the marker for rich internal events recorded by the
// aggregate; the persistence adapter drains them with PullDomainEvents
// and maps them to flat integration events (events/ V1) inside the same
// transaction (ARCHITECTURE.md §4.1).
type DomainEvent interface {
	isDomainEvent()
}

// ParticipantRegistered is recorded by Register. It carries no
// timestamp by design: Register takes no clock (§2.3 signature), so the
// adapter stamps OccurredAt at persist time.
type ParticipantRegistered struct {
	ID          ParticipantID
	Email       EmailAddress
	DisplayName string
}

func (ParticipantRegistered) isDomainEvent() {}

// ParticipantVerified is recorded by Verify with the business time the
// verification happened.
type ParticipantVerified struct {
	ID         ParticipantID
	OccurredAt time.Time
}

func (ParticipantVerified) isDomainEvent() {}
