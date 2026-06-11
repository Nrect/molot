package adapters

import (
	"context"
	"fmt"
	"sync"

	"molot/internal/participant/domain/participant"
)

// InMemoryRepository is the map-of-values implementation of the
// repository port (BOOK_AUDIT rule 20): domain-first development and
// unit tests run against it, the shared suite keeps it behaviorally
// identical to Postgres. Domain events are drained and dropped — there
// is no outbox here.
type InMemoryRepository struct {
	mu           sync.RWMutex
	participants map[participant.ParticipantID]participant.Participant
}

func NewInMemoryRepository() *InMemoryRepository {
	return &InMemoryRepository{participants: make(map[participant.ParticipantID]participant.Participant)}
}

func (r *InMemoryRepository) Add(_ context.Context, p *participant.Participant) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.participants[p.ID()]; ok {
		return fmt.Errorf("add participant %s: %w", p.ID(), participant.ErrEmailTaken)
	}
	for _, existing := range r.participants {
		if existing.Email() == p.Email() {
			return fmt.Errorf("add participant %s: %w", p.ID(), participant.ErrEmailTaken)
		}
	}

	p.PullDomainEvents() // drained as the pg adapter does; no bus in memory
	r.participants[p.ID()] = *p
	return nil
}

func (r *InMemoryRepository) Get(_ context.Context, id participant.ParticipantID) (*participant.Participant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stored, ok := r.participants[id]
	if !ok {
		return nil, participant.NotFoundError{ID: id}
	}
	copied := stored
	return &copied, nil
}

func (r *InMemoryRepository) Update(
	ctx context.Context,
	id participant.ParticipantID,
	actor participant.Actor,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	return r.update(ctx, id, func(p *participant.Participant) error {
		return participant.CanActorUpdateParticipant(actor, *p)
	}, updateFn)
}

func (r *InMemoryRepository) UpdateAsOperations(
	ctx context.Context,
	id participant.ParticipantID,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	return r.update(ctx, id, nil, updateFn)
}

func (r *InMemoryRepository) update(
	ctx context.Context,
	id participant.ParticipantID,
	guard func(p *participant.Participant) error,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.participants[id]
	if !ok {
		return participant.NotFoundError{ID: id}
	}
	working := stored // mutate a copy: an updateFn error must not leak changes ("rollback")

	if guard != nil {
		if err := guard(&working); err != nil {
			return err
		}
	}

	updated, err := updateFn(ctx, &working)
	if err != nil {
		return err
	}

	updated.PullDomainEvents()
	// Re-assemble with the incremented version: the adapter owns the
	// version counter, exactly like the UPDATE ... version+1 in Postgres.
	persisted, err := participant.UnmarshalFromDatabase(
		updated.ID().String(),
		updated.Email().String(),
		updated.DisplayName(),
		updated.Status().String(),
		stored.Version()+1,
	)
	if err != nil {
		return fmt.Errorf("unable to persist updated participant: %w", err)
	}

	r.participants[id] = *persisted
	return nil
}
