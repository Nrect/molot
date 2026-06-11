// Package participant is the domain of the participant bounded context:
// registration and verification of auction participants
// (ARCHITECTURE.md §2.3). The package is stdlib-only and free of
// db/json/transport concerns (BOOK_AUDIT rules 8–14).
package participant

import (
	"fmt"
	"strings"
	"time"
)

// Participant is the aggregate root. All fields are unexported; the only
// ways in are the Register factory and UnmarshalFromDatabase (adapters).
type Participant struct {
	id          ParticipantID
	email       EmailAddress
	displayName string
	status      Status
	version     int64
	events      []DomainEvent
}

// Register creates a new participant in the registered (unverified)
// state and records ParticipantRegistered. Every zero/empty argument is
// rejected — validation happens here and nowhere above.
func Register(id ParticipantID, email EmailAddress, displayName string) (*Participant, error) {
	if id.IsZero() {
		return nil, ErrInvalidParticipantID
	}
	if email.IsZero() {
		return nil, ErrInvalidEmail
	}
	name := strings.TrimSpace(displayName)
	if name == "" {
		return nil, ErrEmptyDisplayName
	}

	p := &Participant{
		id:          id,
		email:       email,
		displayName: name,
		status:      StatusRegistered,
		version:     1,
	}
	p.record(ParticipantRegistered{ID: id, Email: email, DisplayName: name})
	return p, nil
}

// UnmarshalFromDatabase reassembles a persisted participant. It is the
// only entry point for adapters (BOOK_AUDIT rule 7 — no db tags on the
// domain type); raw values go through the same validating constructors
// as user input. No domain events are recorded.
func UnmarshalFromDatabase(id, email, displayName, status string, version int64) (*Participant, error) {
	participantID, err := NewParticipantID(id)
	if err != nil {
		return nil, fmt.Errorf("unmarshal participant id: %w", err)
	}
	emailAddress, err := NewEmailAddress(email)
	if err != nil {
		return nil, fmt.Errorf("unmarshal participant email: %w", err)
	}
	participantStatus, err := NewStatusFromString(status)
	if err != nil {
		return nil, fmt.Errorf("unmarshal participant status: %w", err)
	}
	if strings.TrimSpace(displayName) == "" {
		return nil, fmt.Errorf("unmarshal participant display name: %w", ErrEmptyDisplayName)
	}
	if version < 1 {
		return nil, fmt.Errorf("unmarshal participant: version %d out of range", version)
	}

	return &Participant{
		id:          participantID,
		email:       emailAddress,
		displayName: displayName,
		status:      participantStatus,
		version:     version,
	}, nil
}

// Verify moves the participant to the verified state, exactly once.
// Repeated verification is a business conflict, not a no-op: the
// operations caller gets ErrAlreadyVerified (HTTP 409, §8).
func (p *Participant) Verify(now time.Time) error {
	if p.status == StatusVerified {
		return ErrAlreadyVerified
	}
	p.status = StatusVerified
	p.record(ParticipantVerified{ID: p.id, OccurredAt: now.UTC()})
	return nil
}

func (p Participant) IsVerified() bool { return p.status == StatusVerified }

func (p Participant) ID() ParticipantID { return p.id }

func (p Participant) Email() EmailAddress { return p.email }

func (p Participant) DisplayName() string { return p.displayName }

func (p Participant) Status() Status { return p.status }

// Version is the optimistic-lock counter; it is mapping state — the
// adapter increments it on every persisted update.
func (p Participant) Version() int64 { return p.version }

// PullDomainEvents drains the recorded events for the adapter to map
// and publish in the persistence transaction.
func (p *Participant) PullDomainEvents() []DomainEvent {
	events := p.events
	p.events = nil
	return events
}

func (p *Participant) record(e DomainEvent) {
	p.events = append(p.events, e)
}
