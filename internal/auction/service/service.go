// Package service is the auction context's composition root: it
// assembles adapters → app.Application (decorated handlers) → ports
// and exposes the sync Facade consumed by the settlement saga through
// its consumer-side gateway interface (ARCHITECTURE §9).
package service

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"molot/internal/auction/adapters"
	"molot/internal/auction/app"
	"molot/internal/auction/app/command"
	"molot/internal/auction/app/query"
	"molot/internal/auction/domain/auction"
	auctionevents "molot/internal/auction/events"
	"molot/internal/auction/ports"
	"molot/internal/common/decorator"
	"molot/internal/common/errs"
	"molot/internal/common/server/httperr"
	cwatermill "molot/internal/common/watermill"
	participantevents "molot/internal/participant/events"
)

// Migrations is the context's goose migration set, rooted at the .sql
// files, ready for goose.NewProvider in the composition root.
var Migrations fs.FS = mustSubFS()

func mustSubFS() fs.FS {
	sub, err := fs.Sub(adapters.Migrations, "migrations")
	if err != nil {
		panic("auction service: migrations FS: " + err.Error())
	}
	return sub
}

// Deps is everything the context needs from the composition root.
// The listing-policy fields become the ListingRules snapshot source
// (§2.1): they are read once per listing, never by running auctions.
type Deps struct {
	DB             *sql.DB
	Logger         *slog.Logger
	MeterProvider  metric.MeterProvider
	TracerProvider trace.TracerProvider

	PlatformCurrency   string        // PLATFORM_CURRENCY
	VerifyAboveMinor   int64         // verified_bid_threshold, minor units
	SnipeWindow        time.Duration // anti-snipe trigger window
	SnipeExtension     time.Duration // extension per snipe bid
	SnipeMaxExtensions int           // cap of extensions per auction

	ClosingPollInterval time.Duration // CLOSING_POLL_INTERVAL
}

type Service struct {
	app      app.Application
	facade   Facade
	worker   *ports.ClosingWorker
	http     ports.HTTPServer
	wmLogger wm.LoggerAdapter

	profiles  *adapters.BidderProfilesPostgres
	catalog   *adapters.CatalogPostgresProjection
	dashboard *adapters.DashboardPostgresProjection
}

func NewService(deps Deps) (*Service, error) {
	if deps.DB == nil || deps.Logger == nil || deps.MeterProvider == nil || deps.TracerProvider == nil {
		return nil, fmt.Errorf("auction service: nil dependency in Deps")
	}

	rules, err := listingRules(deps)
	if err != nil {
		return nil, err
	}

	wmLogger := cwatermill.NewLogger(deps.Logger)

	// Initialize the outbox topic schema up front: the TxPublisher
	// deliberately cannot do it (a CREATE TABLE would implicitly
	// commit the business transaction).
	if err := initializeOutboxTopic(deps.DB, wmLogger); err != nil {
		return nil, err
	}

	repo := adapters.NewAuctionPostgresRepository(deps.DB, wmLogger)
	readModels := adapters.NewAuctionPostgresReadModels(deps.DB)
	profiles := adapters.NewBidderProfilesPostgres(deps.DB)
	catalog := adapters.NewCatalogPostgresProjection(deps.DB)
	dashboard := adapters.NewDashboardPostgresProjection(deps.DB)

	return newService(deps, rules, repo, repo, readModels, profiles, catalog, dashboard, wmLogger)
}

// newService is the shared assembly path (rule 44): production and
// component-test composition roots differ only in adapters.
func newService(
	deps Deps,
	rules auction.ListingRules,
	repo auction.Repository,
	scanner interface {
		DueForClosing(ctx context.Context, before time.Time, limit int) ([]auction.AuctionID, error)
	},
	readModels *adapters.AuctionPostgresReadModels,
	profiles *adapters.BidderProfilesPostgres,
	catalog *adapters.CatalogPostgresProjection,
	dashboard *adapters.DashboardPostgresProjection,
	wmLogger wm.LoggerAdapter,
) (*Service, error) {
	decorators, err := decorator.NewDecorators("auction", deps.Logger, deps.MeterProvider, deps.TracerProvider)
	if err != nil {
		return nil, fmt.Errorf("auction service: decorators: %w", err)
	}
	clk := systemClock{}

	application := app.Application{
		Commands: app.Commands{
			ListAuction: decorator.ApplyCommandDecorators(
				command.NewListAuctionHandler(repo, rules, clk), decorators),
			PlaceBid: decorator.ApplyCommandDecorators(
				command.NewPlaceBidHandler(repo, profiles, clk), decorators),
			CancelAuction: decorator.ApplyCommandDecorators(
				command.NewCancelAuctionHandler(repo, clk), decorators),
			CloseAuction: decorator.ApplyCommandDecorators(
				command.NewCloseAuctionHandler(repo, clk), decorators),
			AwardToRunnerUp: decorator.ApplyCommandDecorators(
				command.NewAwardToRunnerUpHandler(repo, clk), decorators),
			RelistAuction: decorator.ApplyCommandDecorators(
				command.NewRelistAuctionHandler(repo, clk), decorators),
			MarkSaleFailed: decorator.ApplyCommandDecorators(
				command.NewMarkSaleFailedHandler(repo, clk), decorators),
			ConfirmSettlement: decorator.ApplyCommandDecorators(
				command.NewConfirmSettlementHandler(repo, clk), decorators),
		},
		Queries: app.Queries{
			ActiveAuctionsCatalog: decorator.ApplyQueryDecorators(
				query.NewActiveAuctionsCatalogHandler(readModels), decorators),
			AuctionCard: decorator.ApplyQueryDecorators(
				query.NewAuctionCardHandler(readModels), decorators),
			BidHistory: decorator.ApplyQueryDecorators(
				query.NewBidHistoryHandler(readModels), decorators),
			SellerDashboard: decorator.ApplyQueryDecorators(
				query.NewSellerDashboardHandler(readModels), decorators),
		},
	}

	interval := deps.ClosingPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	worker := ports.NewClosingWorker(application.Commands.CloseAuction, scanner, clk, interval, deps.Logger,
		deps.TracerProvider, deps.MeterProvider)

	return &Service{
		app: application,
		facade: Facade{
			award:      application.Commands.AwardToRunnerUp,
			relist:     application.Commands.RelistAuction,
			markFailed: application.Commands.MarkSaleFailed,
			confirm:    application.Commands.ConfirmSettlement,
		},
		worker:    worker,
		http:      ports.NewHTTPServer(application),
		wmLogger:  wmLogger,
		profiles:  profiles,
		catalog:   catalog,
		dashboard: dashboard,
	}, nil
}

// Facade returns the sync command surface for the settlement saga.
func (s *Service) Facade() Facade { return s.facade }

// Application exposes the use-case catalog (component tests).
func (s *Service) Application() app.Application { return s.app }

// RegisterHTTP mounts the strict-server handlers on the authenticated
// /api router group. Slug errors are translated by the one common
// helper (rule 30).
func (s *Service) RegisterHTTP(api chi.Router) {
	strictHandler := ports.NewStrictHandlerWithOptions(s.http, nil, ports.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httperr.RespondWithSlugError(errs.NewIncorrectInputError("invalid-request").WithCause(err), w, r)
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httperr.RespondWithSlugError(err, w, r)
		},
	})
	ports.HandlerFromMux(strictHandler, api)
}

// RegisterEventHandlers subscribes the context's typed event handlers
// on the shared router: one consumer group per handler (§4.4).
func (s *Service) RegisterEventHandlers(
	router *message.Router,
	subscriberConstructor func(handlerName string) (message.Subscriber, error),
) error {
	processor, err := cwatermill.NewEventProcessor(router, topicForEvent, subscriberConstructor, s.wmLogger)
	if err != nil {
		return fmt.Errorf("auction service: event processor: %w", err)
	}
	if err := ports.RegisterEventHandlers(processor, s.profiles, s.catalog, s.dashboard); err != nil {
		return fmt.Errorf("auction service: register event handlers: %w", err)
	}
	return nil
}

// ClosingWorker returns the errgroup-ready runner of the by-time
// closing loop (§10).
func (s *Service) ClosingWorker(ctx context.Context) func() error {
	return func() error { return s.worker.Run(ctx) }
}

// topicForEvent maps a marshaled event name to its topic. Event names
// render as "events.<Type>V1" (cqrs.StructName), and both contexts'
// packages are named events — so the type suffix decides.
func topicForEvent(eventName string) string {
	switch {
	case strings.HasSuffix(eventName, "ParticipantRegisteredV1"),
		strings.HasSuffix(eventName, "ParticipantVerifiedV1"):
		return participantevents.Topic
	default:
		return auctionevents.Topic
	}
}

// initializeOutboxTopic creates the watermill schema for the auction
// topic before the first transactional publish.
func initializeOutboxTopic(db *sql.DB, wmLogger wm.LoggerAdapter) error {
	subscriber, err := cwatermill.NewSQLSubscriber(db, "auction.outbox-init", time.Second, wmLogger)
	if err != nil {
		return fmt.Errorf("auction service: outbox init subscriber: %w", err)
	}
	defer func() { _ = subscriber.Close() }()

	initializer, ok := subscriber.(interface{ SubscribeInitialize(topic string) error })
	if !ok {
		return fmt.Errorf("auction service: subscriber cannot initialize schema")
	}
	if err := initializer.SubscribeInitialize(auctionevents.Topic); err != nil {
		return fmt.Errorf("auction service: initialize outbox topic: %w", err)
	}
	return nil
}

// listingRules builds the platform-policy snapshot source from env-fed
// deps; it fails fast on an invalid policy so a misconfigured monolith
// never starts.
func listingRules(deps Deps) (auction.ListingRules, error) {
	currency, err := auction.NewCurrency(deps.PlatformCurrency)
	if err != nil {
		return auction.ListingRules{}, fmt.Errorf("auction service: platform currency: %w", err)
	}
	verifyAbove, err := auction.NewMoney(deps.VerifyAboveMinor, currency)
	if err != nil {
		return auction.ListingRules{}, fmt.Errorf("auction service: verify-above threshold: %w", err)
	}
	antiSnipe, err := auction.NewAntiSnipePolicy(deps.SnipeWindow, deps.SnipeExtension, deps.SnipeMaxExtensions)
	if err != nil {
		return auction.ListingRules{}, fmt.Errorf("auction service: anti-snipe policy: %w", err)
	}
	rules, err := auction.NewListingRules(currency, verifyAbove, antiSnipe)
	if err != nil {
		return auction.ListingRules{}, fmt.Errorf("auction service: listing rules: %w", err)
	}
	return rules, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
