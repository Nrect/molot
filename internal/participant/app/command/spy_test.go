package command_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"molot/internal/participant/domain/participant"
)

// spyRepository is a hand-written recording spy (BOOK_AUDIT rule 41):
// it records which repository methods the handler orchestrated and
// applies updateFn to a held aggregate — no framework, no expectations.
type spyRepository struct {
	added []*participant.Participant

	stored   *participant.Participant
	storeErr error

	updateCalls             int
	updateAsOperationsCalls int
}

func (s *spyRepository) Add(_ context.Context, p *participant.Participant) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	s.added = append(s.added, p)
	return nil
}

func (s *spyRepository) Get(_ context.Context, id participant.ParticipantID) (*participant.Participant, error) {
	if s.stored == nil {
		return nil, participant.NotFoundError{ID: id}
	}
	return s.stored, nil
}

func (s *spyRepository) Update(
	ctx context.Context,
	id participant.ParticipantID,
	_ participant.Actor,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	s.updateCalls++
	return s.runUpdate(ctx, id, updateFn)
}

func (s *spyRepository) UpdateAsOperations(
	ctx context.Context,
	id participant.ParticipantID,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	s.updateAsOperationsCalls++
	return s.runUpdate(ctx, id, updateFn)
}

func (s *spyRepository) runUpdate(
	ctx context.Context,
	id participant.ParticipantID,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	if s.stored == nil {
		return participant.NotFoundError{ID: id}
	}
	updated, err := updateFn(ctx, s.stored)
	if err != nil {
		return err
	}
	s.stored = updated
	return nil
}

// fixedClock is the deterministic consumer-side clock of the tests.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func mustID(t *testing.T, raw string) participant.ParticipantID {
	t.Helper()
	id, err := participant.NewParticipantID(raw)
	require.NoError(t, err)
	return id
}

func mustEmail(t *testing.T, raw string) participant.EmailAddress {
	t.Helper()
	email, err := participant.NewEmailAddress(raw)
	require.NoError(t, err)
	return email
}
