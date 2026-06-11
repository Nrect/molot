// Package service is the settlement context's composition root: it
// assembles adapters → app.Application (decorated handlers) + the saga
// event handlers → ports, with the foreign sync facades arriving from
// the monolith's composition root (ARCHITECTURE §6, §9).
package service

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	auctionevents "molot/internal/auction/events"
	billingevents "molot/internal/billing/events"
	"molot/internal/common/decorator"
	cwatermill "molot/internal/common/watermill"
	"molot/internal/settlement/adapters"
	"molot/internal/settlement/app"
	"molot/internal/settlement/app/command"
	"molot/internal/settlement/app/query"
	"molot/internal/settlement/domain/settlement"
	"molot/internal/settlement/ports"
)

// Migrations is the context's goose migration set, rooted at the .sql
// files, ready for goose.NewProvider in the composition root
// (per-context version table goose_db_version_settlement); the raw
// embed.FS is adapters.Migrations.
var Migrations fs.FS = mustSubFS()

func mustSubFS() fs.FS {
	sub, err := fs.Sub(adapters.Migrations, "migrations")
	if err != nil {
		panic("settlement service: migrations FS: " + err.Error())
	}
	return sub
}

// Deps is everything the context needs from the composition root.
// AuctionFacade/BillingFacade are the synchronous saga surfaces of the
// other contexts — main passes auctionservice.Facade and
// billingservice.Facade values (ADR-0004); the interface types live in
// adapters, so no layer here imports a foreign service package.
type Deps struct {
	DB             *sql.DB
	Logger         *slog.Logger
	MeterProvider  metric.MeterProvider
	TracerProvider trace.TracerProvider

	AuctionFacade adapters.AuctionFacade
	BillingFacade adapters.BillingFacade

	RelistDelay    time.Duration // RELIST_DELAY: replacement listing starts at now+delay
	RelistDuration time.Duration // RELIST_DURATION: replacement bidding window length
}

// Service is the settlement context plugged into the monolith.
type Service struct {
	app      app.Application
	handlers app.EventHandlers
	logger   *slog.Logger
}

func NewService(deps Deps) (*Service, error) {
	if deps.DB == nil {
		panic("settlement.NewService: nil db")
	}
	if deps.Logger == nil {
		panic("settlement.NewService: nil logger")
	}
	if deps.MeterProvider == nil {
		panic("settlement.NewService: nil meter provider")
	}
	if deps.TracerProvider == nil {
		panic("settlement.NewService: nil tracer provider")
	}
	if deps.AuctionFacade == nil {
		panic("settlement.NewService: nil auction facade")
	}
	if deps.BillingFacade == nil {
		panic("settlement.NewService: nil billing facade")
	}

	relist, err := settlement.NewRelistPolicy(deps.RelistDelay, deps.RelistDuration)
	if err != nil {
		return nil, fmt.Errorf("settlement service: relist policy: %w", err)
	}
	decorators, err := decorator.NewDecorators("settlement", deps.Logger, deps.MeterProvider, deps.TracerProvider)
	if err != nil {
		return nil, fmt.Errorf("settlement service: decorators: %w", err)
	}

	repo := adapters.NewSettlementPostgresRepository(deps.DB, deps.MeterProvider)
	// molot_settlement_nonterminal_age_seconds (§11): observable-gauge
	// scan of settlement.settlements — the "stuck saga" alarm.
	if err := adapters.RegisterNonterminalAgeGauge(deps.DB, deps.MeterProvider); err != nil {
		return nil, fmt.Errorf("settlement service: %w", err)
	}
	auctionGateway := adapters.NewAuctionFacadeAdapter(deps.AuctionFacade, deps.TracerProvider)
	billingGateway := adapters.NewBillingFacadeAdapter(deps.BillingFacade, deps.TracerProvider)
	clk := systemClock{}

	application := app.Application{
		Commands: app.Commands{
			DeclineSecondChanceOffer: decorator.ApplyCommandDecorators(
				command.NewDeclineSecondChanceOfferHandler(repo, auctionGateway, billingGateway, relist, clk),
				decorators),
		},
		Queries: app.Queries{
			SettlementStatus: decorator.ApplyQueryDecorators(
				query.NewSettlementStatusHandler(repo), decorators),
		},
	}

	return &Service{
		app:      application,
		handlers: app.NewEventHandlers(repo, auctionGateway, billingGateway, relist, clk),
		logger:   deps.Logger,
	}, nil
}

// Application exposes the use-case catalog (component tests).
func (s *Service) Application() app.Application { return s.app }

// RegisterHTTP mounts the strict-server handlers on the authenticated
// /api router group.
func (s *Service) RegisterHTTP(api chi.Router) {
	ports.RegisterHTTP(api, s.app)
}

// RegisterEventHandlers subscribes the saga's typed event handlers on
// the shared router: one consumer group per handler (§4.4). Settlement
// publishes no integration events of its own — there is no outbox to
// initialize here.
func (s *Service) RegisterEventHandlers(
	router *message.Router,
	subscriberConstructor func(handlerName string) (message.Subscriber, error),
) error {
	processor, err := cwatermill.NewEventProcessor(
		router, topicForEvent, subscriberConstructor, cwatermill.NewLogger(s.logger))
	if err != nil {
		return fmt.Errorf("settlement service: event processor: %w", err)
	}
	if err := ports.RegisterEventHandlers(processor, s.handlers); err != nil {
		return fmt.Errorf("settlement service: register event handlers: %w", err)
	}
	return nil
}

// topicForEvent maps a marshaled event name to its topic. Event names
// render as "events.<Type>V1" (cqrs.StructName) and both producer
// packages are named events — the type suffix decides.
func topicForEvent(eventName string) string {
	switch {
	case strings.HasSuffix(eventName, "InvoicePaidV1"),
		strings.HasSuffix(eventName, "InvoiceExpiredV1"):
		return billingevents.Topic
	default:
		return auctionevents.Topic
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
