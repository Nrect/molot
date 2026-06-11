// Package service is the composition root of the participant context:
// NewService assembles adapters → app.Application (decorated handlers)
// → HTTP port, and exposes registration hooks for main
// (ARCHITECTURE.md §9).
//
// The context publishes ParticipantRegisteredV1/ParticipantVerifiedV1
// and subscribes to nothing, so there is no RegisterEventHandlers; no
// other context calls participant synchronously, so there is no Facade.
package service

import (
	"database/sql"
	"io/fs"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"molot/internal/common/decorator"
	cwatermill "molot/internal/common/watermill"
	"molot/internal/participant/adapters"
	"molot/internal/participant/app"
	"molot/internal/participant/app/command"
	"molot/internal/participant/app/query"
	"molot/internal/participant/ports"
)

// Migrations are the embedded goose migrations of the context; main
// appends them to the monolith's migration list (per-context goose
// version table — see runMigrations in cmd/monolith).
var Migrations fs.FS = adapters.Migrations

// Service wires the participant context into the monolith.
type Service struct {
	app        app.Application
	httpServer ports.HTTPServer
}

// NewService builds the production composition of the context. It also
// initializes the watermill schema of the participant-events topic so
// the transactional outbox can publish before any subscriber exists.
func NewService(
	db *sql.DB,
	logger *slog.Logger,
	meterProvider metric.MeterProvider,
	tracerProvider trace.TracerProvider,
) (*Service, error) {
	if db == nil {
		panic("participant.NewService: nil db")
	}
	if logger == nil {
		panic("participant.NewService: nil logger")
	}
	if meterProvider == nil {
		panic("participant.NewService: nil meter provider")
	}
	if tracerProvider == nil {
		panic("participant.NewService: nil tracer provider")
	}

	wmLogger := cwatermill.NewLogger(logger)
	if err := adapters.InitializeEventsSchema(db, wmLogger); err != nil {
		return nil, err
	}

	decorators, err := decorator.NewDecorators("participant", logger, meterProvider, tracerProvider)
	if err != nil {
		return nil, err
	}

	repo := adapters.NewPostgresRepository(db, wmLogger)
	readModels := adapters.NewPostgresReadModels(db)

	application := app.Application{
		Commands: app.Commands{
			RegisterParticipant: decorator.ApplyCommandDecorators(
				command.NewRegisterParticipantHandler(repo), decorators),
			VerifyParticipant: decorator.ApplyCommandDecorators(
				command.NewVerifyParticipantHandler(repo, systemClock{}), decorators),
		},
		Queries: app.Queries{
			ParticipantProfile: decorator.ApplyQueryDecorators(
				query.NewParticipantProfileHandler(readModels), decorators),
		},
	}

	return &Service{
		app:        application,
		httpServer: ports.NewHTTPServer(application),
	}, nil
}

// RegisterHTTP mounts the JWT-protected endpoints on the /api group:
// GET /api/participants/{participantID} and
// POST /api/participants/{participantID}/verification.
func (s *Service) RegisterHTTP(api chi.Router) {
	ports.RegisterAuthenticatedRoutes(api, s.httpServer)
}

// RegisterPublicHTTP mounts POST /participants on the root router.
// Registration is public by design (§8: "registration IS the entry"),
// and the common /api group unconditionally enforces JWT — per
// server.NewRouter's contract, unauthenticated routes live on the root.
func (s *Service) RegisterPublicHTTP(public chi.Router) {
	ports.RegisterPublicRoutes(public, s.httpServer)
}

// systemClock is the production clock of the command layer.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
