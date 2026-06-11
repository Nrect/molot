package command_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/errs"
	"molot/internal/participant/app/command"
	"molot/internal/participant/domain/participant"
)

const participantID = "0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f"

func TestRegisterParticipantHandler(t *testing.T) {
	t.Parallel()

	t.Run("registers the participant built from the command", func(t *testing.T) {
		t.Parallel()

		repo := &spyRepository{}
		handler := command.NewRegisterParticipantHandler(repo)

		cmd := command.RegisterParticipant{
			ID:          mustID(t, participantID),
			Email:       mustEmail(t, "boris@example.com"),
			DisplayName: "Boris",
		}
		require.NoError(t, handler.Handle(t.Context(), cmd))

		require.Len(t, repo.added, 1)
		added := repo.added[0]
		assert.Equal(t, cmd.ID, added.ID())
		assert.Equal(t, cmd.Email, added.Email())
		assert.Equal(t, "Boris", added.DisplayName())
		assert.False(t, added.IsVerified())
	})

	t.Run("taken email maps to the email-taken conflict slug", func(t *testing.T) {
		t.Parallel()

		repo := &spyRepository{storeErr: participant.ErrEmailTaken}
		handler := command.NewRegisterParticipantHandler(repo)

		err := handler.Handle(t.Context(), command.RegisterParticipant{
			ID:          mustID(t, participantID),
			Email:       mustEmail(t, "boris@example.com"),
			DisplayName: "Boris",
		})

		require.ErrorIs(t, err, errs.NewConflictError("email-taken"))
		require.ErrorIs(t, err, participant.ErrEmailTaken, "the cause is preserved for logs")
	})

	t.Run("domain validation failure maps to 400 and skips the repository", func(t *testing.T) {
		t.Parallel()

		repo := &spyRepository{}
		handler := command.NewRegisterParticipantHandler(repo)

		err := handler.Handle(t.Context(), command.RegisterParticipant{
			ID:          mustID(t, participantID),
			Email:       mustEmail(t, "boris@example.com"),
			DisplayName: "   ",
		})

		require.ErrorIs(t, err, errs.NewIncorrectInputError("invalid-display-name"))
		require.ErrorIs(t, err, participant.ErrEmptyDisplayName)
		assert.Empty(t, repo.added, "nothing must reach the repository")
	})

	t.Run("unexpected repository error passes through unwrapped", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("connection reset")
		repo := &spyRepository{storeErr: boom}
		handler := command.NewRegisterParticipantHandler(repo)

		err := handler.Handle(t.Context(), command.RegisterParticipant{
			ID:          mustID(t, participantID),
			Email:       mustEmail(t, "boris@example.com"),
			DisplayName: "Boris",
		})

		require.ErrorIs(t, err, boom)
		assert.Equal(t, errs.ErrorKindUnknown, errs.KindFromError(err), "infrastructure errors stay 500")
	})

	t.Run("constructor panics on nil repo", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() { command.NewRegisterParticipantHandler(nil) })
	})
}
