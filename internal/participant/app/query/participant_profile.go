package query

import (
	"context"
	"errors"

	"molot/internal/common/errs"
	"molot/internal/participant/domain/participant"
)

// ParticipantProfile asks for one participant's public profile.
type ParticipantProfile struct {
	ID participant.ParticipantID
}

// ProfileView is the UI-shaped read result (not domain, not OpenAPI,
// not a DB model — BOOK_AUDIT rule 26). It is served by a direct read
// of the write table (§5: not every query needs a projection).
type ProfileView struct {
	ParticipantID string
	Email         string
	DisplayName   string
	Status        string
}

// ProfileReadModel is the consumer-side read port, declared next to the
// query that needs it (rule 32); the Postgres adapter implements it.
type ProfileReadModel interface {
	Profile(ctx context.Context, id participant.ParticipantID) (ProfileView, error)
}

type ParticipantProfileHandler struct {
	readModel ProfileReadModel
}

func NewParticipantProfileHandler(readModel ProfileReadModel) ParticipantProfileHandler {
	if readModel == nil {
		panic("NewParticipantProfileHandler: nil read model")
	}
	return ParticipantProfileHandler{readModel: readModel}
}

func (h ParticipantProfileHandler) Handle(ctx context.Context, q ParticipantProfile) (ProfileView, error) {
	view, err := h.readModel.Profile(ctx, q.ID)
	if err != nil {
		var notFound participant.NotFoundError
		if errors.As(err, &notFound) {
			return ProfileView{}, errs.NewNotFoundError("participant-not-found").WithCause(err)
		}
		return ProfileView{}, err
	}
	return view, nil
}
