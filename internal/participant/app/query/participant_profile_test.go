package query_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/errs"
	"molot/internal/participant/app/query"
	"molot/internal/participant/domain/participant"
)

// spyReadModel is a recording spy for the consumer-side read port.
type spyReadModel struct {
	view query.ProfileView
	err  error

	requested []participant.ParticipantID
}

func (s *spyReadModel) Profile(_ context.Context, id participant.ParticipantID) (query.ProfileView, error) {
	s.requested = append(s.requested, id)
	if s.err != nil {
		return query.ProfileView{}, s.err
	}
	return s.view, nil
}

func TestParticipantProfileHandler(t *testing.T) {
	t.Parallel()

	id, err := participant.NewParticipantID("0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f")
	require.NoError(t, err)

	t.Run("passes the view through untouched", func(t *testing.T) {
		t.Parallel()

		want := query.ProfileView{
			ParticipantID: id.String(),
			Email:         "boris@example.com",
			DisplayName:   "Boris",
			Status:        "verified",
		}
		readModel := &spyReadModel{view: want}
		handler := query.NewParticipantProfileHandler(readModel)

		got, err := handler.Handle(t.Context(), query.ParticipantProfile{ID: id})
		require.NoError(t, err)

		assert.Equal(t, want, got)
		assert.Equal(t, []participant.ParticipantID{id}, readModel.requested)
	})

	t.Run("missing participant maps to the not-found slug", func(t *testing.T) {
		t.Parallel()

		readModel := &spyReadModel{err: participant.NotFoundError{ID: id}}
		handler := query.NewParticipantProfileHandler(readModel)

		_, err := handler.Handle(t.Context(), query.ParticipantProfile{ID: id})

		require.ErrorIs(t, err, errs.NewNotFoundError("participant-not-found"))
	})

	t.Run("infrastructure errors pass through as 500", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("connection reset")
		handler := query.NewParticipantProfileHandler(&spyReadModel{err: boom})

		_, err := handler.Handle(t.Context(), query.ParticipantProfile{ID: id})

		require.ErrorIs(t, err, boom)
		assert.Equal(t, errs.ErrorKindUnknown, errs.KindFromError(err))
	})

	t.Run("constructor panics on nil read model", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() { query.NewParticipantProfileHandler(nil) })
	})
}
