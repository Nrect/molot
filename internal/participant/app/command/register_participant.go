package command

import (
	"context"
	"errors"

	"molot/internal/common/errs"
	"molot/internal/participant/domain/participant"
)

// RegisterParticipant registers a new participant. The UUID is
// client-generated (§8); the struct carries domain types (BOOK_AUDIT
// rule 26) — ports construct them and surface construction failures as
// 400 slugs.
type RegisterParticipant struct {
	ID          participant.ParticipantID
	Email       participant.EmailAddress
	DisplayName string
}

type RegisterParticipantHandler struct {
	repo participant.Repository
}

func NewRegisterParticipantHandler(repo participant.Repository) RegisterParticipantHandler {
	if repo == nil {
		panic("NewRegisterParticipantHandler: nil repo")
	}
	return RegisterParticipantHandler{repo: repo}
}

func (h RegisterParticipantHandler) Handle(ctx context.Context, cmd RegisterParticipant) error {
	p, err := participant.Register(cmd.ID, cmd.Email, cmd.DisplayName)
	if err != nil {
		return registrationInputError(err)
	}

	if err := h.repo.Add(ctx, p); err != nil {
		if errors.Is(err, participant.ErrEmailTaken) {
			return errs.NewConflictError("email-taken").WithCause(err)
		}
		return err
	}
	return nil
}

// registrationInputError translates Register's validation sentinels into
// the stable client slugs of §8.
func registrationInputError(err error) error {
	switch {
	case errors.Is(err, participant.ErrInvalidEmail):
		return errs.NewIncorrectInputError("invalid-email").WithCause(err)
	case errors.Is(err, participant.ErrInvalidParticipantID):
		return errs.NewIncorrectInputError("invalid-participant-id").WithCause(err)
	case errors.Is(err, participant.ErrEmptyDisplayName):
		return errs.NewIncorrectInputError("invalid-display-name").WithCause(err)
	default:
		return errs.NewIncorrectInputError("invalid-registration").WithCause(err)
	}
}
