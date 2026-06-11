// HTTP port of the participant context: oapi-codegen strict handlers
// over app.Application. Ports only call command/query handlers (rule 28)
// and translate every error through the single httperr helper (rule 30).
package ports

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"molot/internal/common/auth"
	"molot/internal/common/errs"
	"molot/internal/common/server/httperr"
	"molot/internal/participant/app"
	"molot/internal/participant/app/command"
	"molot/internal/participant/app/query"
	"molot/internal/participant/domain/participant"
)

// HTTPServer implements StrictServerInterface on top of the use-case
// catalog.
type HTTPServer struct {
	app app.Application
}

func NewHTTPServer(application app.Application) HTTPServer {
	if application.Commands.RegisterParticipant == nil ||
		application.Commands.VerifyParticipant == nil ||
		application.Queries.ParticipantProfile == nil {
		panic("NewHTTPServer: incomplete app.Application")
	}
	return HTTPServer{app: application}
}

// RegisterAuthenticatedRoutes mounts the JWT-protected endpoints on the
// /api group (relative paths — the group itself lives under /api).
func RegisterAuthenticatedRoutes(api chi.Router, server HTTPServer) {
	wrapper := newServerWrapper(server)
	api.Get("/participants/{participantID}", wrapper.ParticipantProfile)
	api.Post("/participants/{participantID}/verification", wrapper.VerifyParticipant)
}

// RegisterPublicRoutes mounts registration on an unauthenticated router
// (the root one): the common /api group enforces JWT on every route,
// and registration is the entry point — there is no token yet.
func RegisterPublicRoutes(public chi.Router, server HTTPServer) {
	wrapper := newServerWrapper(server)
	public.Post("/participants", wrapper.RegisterParticipant)
}

// newServerWrapper builds the generated chi wrapper around the strict
// handler, with all error paths funnelled into httperr: malformed
// requests become 400 {"slug":"invalid-request"}, handler errors are
// translated by their errs.ErrorKind.
func newServerWrapper(server HTTPServer) ServerInterfaceWrapper {
	requestError := func(w http.ResponseWriter, r *http.Request, err error) {
		httperr.RespondWithSlugError(errs.NewIncorrectInputError("invalid-request").WithCause(err), w, r)
	}
	strict := NewStrictHandlerWithOptions(server, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: requestError,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httperr.RespondWithSlugError(err, w, r)
		},
	})
	return ServerInterfaceWrapper{Handler: strict, ErrorHandlerFunc: requestError}
}

func (s HTTPServer) RegisterParticipant(
	ctx context.Context, request RegisterParticipantRequestObject,
) (RegisterParticipantResponseObject, error) {
	id, err := participant.NewParticipantID(request.Body.Id)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-participant-id").WithCause(err)
	}
	email, err := participant.NewEmailAddress(request.Body.Email)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-email").WithCause(err)
	}

	cmd := command.RegisterParticipant{ID: id, Email: email, DisplayName: request.Body.DisplayName}
	if err := s.app.Commands.RegisterParticipant.Handle(ctx, cmd); err != nil {
		return nil, err
	}

	location := "/api/participants/" + id.String()
	return RegisterParticipant204Response{
		Headers: RegisterParticipant204ResponseHeaders{ContentLocation: &location},
	}, nil
}

func (s HTTPServer) ParticipantProfile(
	ctx context.Context, request ParticipantProfileRequestObject,
) (ParticipantProfileResponseObject, error) {
	id, err := participant.NewParticipantID(request.ParticipantID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-participant-id").WithCause(err)
	}

	view, err := s.app.Queries.ParticipantProfile.Handle(ctx, query.ParticipantProfile{ID: id})
	if err != nil {
		return nil, err
	}

	return ParticipantProfile200JSONResponse{
		ParticipantId: view.ParticipantID,
		Email:         view.Email,
		DisplayName:   view.DisplayName,
		Status:        ProfileResponseStatus(view.Status),
	}, nil
}

func (s HTTPServer) VerifyParticipant(
	ctx context.Context, request VerifyParticipantRequestObject,
) (VerifyParticipantResponseObject, error) {
	user, err := auth.UserFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if user.Role != auth.RoleOperations {
		return nil, errs.NewForbiddenError("operations-only")
	}

	id, err := participant.NewParticipantID(request.ParticipantID)
	if err != nil {
		return nil, errs.NewIncorrectInputError("invalid-participant-id").WithCause(err)
	}

	if err := s.app.Commands.VerifyParticipant.Handle(ctx, command.VerifyParticipant{ID: id}); err != nil {
		return nil, err
	}
	return VerifyParticipant204Response{}, nil
}
