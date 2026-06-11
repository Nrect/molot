package command

import (
	"context"
	"errors"
	"time"

	"molot/internal/common/errs"
	"molot/internal/participant/domain/participant"
)

// clock is the consumer-side time source of the command layer
// (ARCHITECTURE.md §10): handlers never call time.Now directly, so unit
// tests stay deterministic.
type clock interface {
	Now() time.Time
}

// VerifyParticipant marks a participant as verified. It is an
// operations-role command (§8); the port gates the role, the handler
// runs the system repository path (rule 23).
type VerifyParticipant struct {
	ID participant.ParticipantID
}

type VerifyParticipantHandler struct {
	repo  participant.Repository
	clock clock
}

func NewVerifyParticipantHandler(repo participant.Repository, clk clock) VerifyParticipantHandler {
	if repo == nil {
		panic("NewVerifyParticipantHandler: nil repo")
	}
	if clk == nil {
		panic("NewVerifyParticipantHandler: nil clock")
	}
	return VerifyParticipantHandler{repo: repo, clock: clk}
}

func (h VerifyParticipantHandler) Handle(ctx context.Context, cmd VerifyParticipant) error {
	err := h.repo.UpdateAsOperations(ctx, cmd.ID,
		func(_ context.Context, p *participant.Participant) (*participant.Participant, error) {
			if err := p.Verify(h.clock.Now()); err != nil {
				return nil, err
			}
			return p, nil
		})

	switch {
	case err == nil:
		return nil
	case errors.Is(err, participant.ErrAlreadyVerified):
		// Repeated verification is a real conflict (409), not a saga-style no-op.
		return errs.NewConflictError("already-verified").WithCause(err)
	default:
		var notFound participant.NotFoundError
		if errors.As(err, &notFound) {
			return errs.NewNotFoundError("participant-not-found").WithCause(err)
		}
		return err
	}
}
