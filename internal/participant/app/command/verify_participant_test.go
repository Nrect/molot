package command_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/errs"
	"molot/internal/participant/app/command"
	"molot/internal/participant/domain/participant"
)

func TestVerifyParticipantHandler(t *testing.T) {
	t.Parallel()

	newRegistered := func(t *testing.T) *participant.Participant {
		t.Helper()
		p, err := participant.Register(mustID(t, participantID), mustEmail(t, "boris@example.com"), "Boris")
		require.NoError(t, err)
		p.PullDomainEvents()
		return p
	}

	t.Run("verifies through the operations path at clock time", func(t *testing.T) {
		t.Parallel()

		now := time.Date(2026, 6, 11, 6, 40, 0, 0, time.UTC)
		repo := &spyRepository{stored: newRegistered(t)}
		handler := command.NewVerifyParticipantHandler(repo, fixedClock{now: now})

		err := handler.Handle(t.Context(), command.VerifyParticipant{ID: mustID(t, participantID)})
		require.NoError(t, err)

		assert.Equal(t, 1, repo.updateAsOperationsCalls, "verification is the system operations path")
		assert.Zero(t, repo.updateCalls, "the user-owned path must not be used")
		assert.True(t, repo.stored.IsVerified())

		events := repo.stored.PullDomainEvents()
		require.Len(t, events, 1)
		verified, ok := events[0].(participant.ParticipantVerified)
		require.True(t, ok, "expected ParticipantVerified, got %T", events[0])
		assert.Equal(t, now, verified.OccurredAt, "domain received the injected clock time")
	})

	t.Run("repeated verification maps to the already-verified conflict slug", func(t *testing.T) {
		t.Parallel()

		p := newRegistered(t)
		require.NoError(t, p.Verify(time.Now()))
		p.PullDomainEvents()
		repo := &spyRepository{stored: p}
		handler := command.NewVerifyParticipantHandler(repo, fixedClock{now: time.Now()})

		err := handler.Handle(t.Context(), command.VerifyParticipant{ID: mustID(t, participantID)})

		require.ErrorIs(t, err, errs.NewConflictError("already-verified"))
		require.ErrorIs(t, err, participant.ErrAlreadyVerified)
	})

	t.Run("missing participant maps to the not-found slug", func(t *testing.T) {
		t.Parallel()

		repo := &spyRepository{}
		handler := command.NewVerifyParticipantHandler(repo, fixedClock{now: time.Now()})

		err := handler.Handle(t.Context(), command.VerifyParticipant{ID: mustID(t, participantID)})

		require.ErrorIs(t, err, errs.NewNotFoundError("participant-not-found"))
	})

	t.Run("constructor panics on nil dependencies", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() { command.NewVerifyParticipantHandler(nil, fixedClock{}) })
		assert.Panics(t, func() { command.NewVerifyParticipantHandler(&spyRepository{}, nil) })
	})
}
