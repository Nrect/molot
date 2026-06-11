// Package monolith contains the shared application assembly used by both
// the production entry point (cmd/monolith/main.go) and the component-test
// composition root (tests/component). The split follows BOOK_AUDIT rule 44:
// NewApplication (prod) and NewComponentTestApplication (test) each delegate
// to the single private newApplication, differing only in which adapters they
// supply.
//
// See ARCHITECTURE.md §9 ("Package layout") for the full wiring picture.
package monolith

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pressly/goose/v3"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sync/errgroup"

	auctionservice "molot/internal/auction/service"
	billingservice "molot/internal/billing/service"
	"molot/internal/common/auth"
	"molot/internal/common/config"
	"molot/internal/common/logs"
	"molot/internal/common/metrics"
	"molot/internal/common/postgres"
	"molot/internal/common/server"
	"molot/internal/common/tracing"
	cwatermill "molot/internal/common/watermill"
	notificationservice "molot/internal/notification/service"
	participantservice "molot/internal/participant/service"
	settlementservice "molot/internal/settlement/service"
)

// Application is the assembled monolith: HTTP handler, a background-workers
// run function and accessors to the context services (used by component tests
// for assertion/introspection).
type Application struct {
	// HTTPHandler is the chi root router; in component tests it is served
	// by httptest.NewServer.
	HTTPHandler http.Handler

	// RunWorkers starts the watermill router, the auction-closing worker
	// and the invoice-expiry worker. The HTTP server is the caller's
	// concern: cmd/monolith serves HTTPHandler itself, component tests
	// use httptest.NewServer.
	RunWorkers func(ctx context.Context) error

	// BusRunning is closed once the watermill router consumes; the prod
	// entry gates its HTTP listener on it so readiness cannot flap.
	BusRunning <-chan struct{}

	// Context-service accessors for component-test introspection.
	AuctionSvc     *auctionservice.Service
	ParticipantSvc *participantservice.Service
	BillingSvc     *billingservice.Service
	SettlementSvc  *settlementservice.Service
}

// NewApplication builds the production monolith. It initialises OTel
// providers, connects to Postgres and calls newApplication. The returned
// shutdown function must be deferred by the caller to flush OTel providers.
func NewApplication(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	db *sql.DB,
) (*Application, func(context.Context), error) {
	tracerProvider, err := tracing.NewTracerProvider(ctx, cfg.OTELExporterOTLPEndpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("init tracing: %w", err)
	}

	meterProvider, err := metrics.NewMeterProvider(ctx, cfg.OTELExporterOTLPEndpoint)
	if err != nil {
		tracerProvider.Shutdown(ctx) //nolint:errcheck
		return nil, nil, fmt.Errorf("init metrics: %w", err)
	}

	authMiddleware, err := auth.NewMiddleware(string(cfg.AuthMode), cfg.AuthHS256Secret)
	if err != nil {
		tracerProvider.Shutdown(ctx) //nolint:errcheck
		meterProvider.Shutdown(ctx)  //nolint:errcheck
		return nil, nil, err
	}

	app, err := newApplication(ctx, cfg, logger, db, authMiddleware, meterProvider, tracerProvider)
	if err != nil {
		tracerProvider.Shutdown(ctx) //nolint:errcheck
		meterProvider.Shutdown(ctx)  //nolint:errcheck
		return nil, nil, err
	}

	shutdown := func(shutCtx context.Context) {
		tracerProvider.Shutdown(shutCtx) //nolint:errcheck
		meterProvider.Shutdown(shutCtx)  //nolint:errcheck
	}
	return app, shutdown, nil
}

// NewComponentTestApplication builds the monolith wired for component tests:
// OTel providers are no-ops (no gRPC collector needed), and the PSP mode is
// overridden so individual test suites can steer payment outcomes. The HTTP
// server is NOT started here — TestMain starts httptest.NewServer(app.HTTPHandler).
//
// Short test-friendly values are injected for all polling intervals; the
// caller controls paymentTerm and pspMode per test suite.
func NewComponentTestApplication(
	ctx context.Context,
	db *sql.DB,
	hs256Secret string,
	platformCurrency string,
	paymentTerm time.Duration,
	pspMode string,
) (*Application, error) {
	logger := logs.NewLogger("text")

	authMiddleware, err := auth.NewMiddleware(auth.ModeLocalHS256, hs256Secret)
	if err != nil {
		return nil, err
	}

	cfg := config.Config{
		PlatformCurrency:      platformCurrency,
		CommissionBasisPoints: 1000,
		VerifyAboveMinor:      100_000,
		SnipeWindow:           5 * time.Second,
		SnipeExtension:        5 * time.Second,
		SnipeMaxExtensions:    3,
		PaymentTerm:           paymentTerm,
		PSPMode:               config.PSPMode(pspMode),
		RelistDelay:           time.Second,
		RelistDuration:        6 * time.Second,
		ClosingPollInterval:   200 * time.Millisecond,
		ExpiryPollInterval:    200 * time.Millisecond,
		BusPollInterval:       50 * time.Millisecond,
		HTTPPort:              0,
		DatabaseURL:           "",
		AuthMode:              config.AuthModeLocalHS256,
		AuthHS256Secret:       hs256Secret,
		LogFormat:             config.LogFormatText,
	}

	return newApplication(ctx, cfg, logger, db, authMiddleware,
		metricnoop.NewMeterProvider(), tracenoop.NewTracerProvider())
}

// newApplication is the single shared wiring path called by both
// NewApplication and NewComponentTestApplication.
func newApplication(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	db *sql.DB,
	authMiddleware func(http.Handler) http.Handler,
	meterProvider metric.MeterProvider,
	tracerProvider trace.TracerProvider,
) (*Application, error) {
	// --- migrations ----------------------------------------------------------
	migrations := []contextMigrations{
		{table: "goose_db_version_participant", fs: participantservice.Migrations},
		{table: "goose_db_version_auction", fs: auctionservice.Migrations},
		{table: "goose_db_version_billing", fs: billingservice.Migrations()},
		{table: "goose_db_version_settlement", fs: settlementservice.Migrations},
		{table: "goose_db_version_notification", fs: notificationservice.Migrations},
	}
	if err := runMigrations(ctx, db, migrations); err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	// --- watermill -----------------------------------------------------------
	wmLogger := cwatermill.NewLogger(logger)

	deadLetterPublisher, err := cwatermill.NewSQLPublisher(db, wmLogger)
	if err != nil {
		return nil, fmt.Errorf("create dead letter publisher: %w", err)
	}

	wmRouter, err := cwatermill.NewRouter(wmLogger, deadLetterPublisher, meterProvider)
	if err != nil {
		return nil, fmt.Errorf("create watermill router: %w", err)
	}

	subscriberConstructor := func(handlerName string) (message.Subscriber, error) {
		return cwatermill.NewSQLSubscriber(db, handlerName, cfg.BusPollInterval, wmLogger)
	}

	if err := cwatermill.RegisterBusMetrics(db, meterProvider); err != nil {
		return nil, fmt.Errorf("register bus metrics: %w", err)
	}

	// --- HTTP router ---------------------------------------------------------
	rootRouter, apiRouter := server.NewRouter(logger, authMiddleware)

	// --- context assembly ----------------------------------------------------
	auctionSvc, err := auctionservice.NewService(auctionservice.Deps{
		DB:                  db,
		Logger:              logger,
		MeterProvider:       meterProvider,
		TracerProvider:      tracerProvider,
		PlatformCurrency:    cfg.PlatformCurrency,
		VerifyAboveMinor:    cfg.VerifyAboveMinor,
		SnipeWindow:         cfg.SnipeWindow,
		SnipeExtension:      cfg.SnipeExtension,
		SnipeMaxExtensions:  cfg.SnipeMaxExtensions,
		ClosingPollInterval: cfg.ClosingPollInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("assemble auction context: %w", err)
	}
	auctionSvc.RegisterHTTP(apiRouter)
	if err := auctionSvc.RegisterEventHandlers(wmRouter, subscriberConstructor); err != nil {
		return nil, fmt.Errorf("register auction event handlers: %w", err)
	}

	participantSvc, err := participantservice.NewService(db, logger, meterProvider, tracerProvider)
	if err != nil {
		return nil, fmt.Errorf("assemble participant context: %w", err)
	}
	participantSvc.RegisterHTTP(apiRouter)
	participantSvc.RegisterPublicHTTP(rootRouter)

	billingSvc, err := billingservice.NewService(billingservice.Deps{
		Logger:                logger,
		DB:                    db,
		WatermillLogger:       wmLogger,
		MeterProvider:         meterProvider,
		TracerProvider:        tracerProvider,
		CommissionBasisPoints: cfg.CommissionBasisPoints,
		PaymentTerm:           cfg.PaymentTerm,
		PSPMode:               string(cfg.PSPMode),
		ExpiryPollInterval:    cfg.ExpiryPollInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("assemble billing context: %w", err)
	}
	billingSvc.RegisterHTTP(apiRouter)

	settlementSvc, err := settlementservice.NewService(settlementservice.Deps{
		DB:             db,
		Logger:         logger,
		MeterProvider:  meterProvider,
		TracerProvider: tracerProvider,
		AuctionFacade:  auctionSvc.Facade(),
		BillingFacade:  billingSvc.Facade(),
		RelistDelay:    cfg.RelistDelay,
		RelistDuration: cfg.RelistDuration,
	})
	if err != nil {
		return nil, fmt.Errorf("assemble settlement context: %w", err)
	}
	settlementSvc.RegisterHTTP(apiRouter)
	if err := settlementSvc.RegisterEventHandlers(wmRouter, subscriberConstructor); err != nil {
		return nil, fmt.Errorf("register settlement event handlers: %w", err)
	}

	notificationSvc := notificationservice.NewService(db, logger)
	if err := notificationSvc.RegisterEventHandlers(wmRouter, subscriberConstructor); err != nil {
		return nil, fmt.Errorf("register notification event handlers: %w", err)
	}

	server.RegisterHealthEndpoints(rootRouter, logger, []server.ReadinessProbe{
		{Name: "postgres", Check: db.PingContext},
		{Name: "watermill-router", Check: func(context.Context) error {
			if !wmRouter.IsRunning() {
				return errors.New("router is not running")
			}
			return nil
		}},
	})

	// --- workers run function ------------------------------------------------
	runWorkers := func(runCtx context.Context) error {
		g, runCtx := errgroup.WithContext(runCtx)

		g.Go(func() error {
			if err := wmRouter.Run(runCtx); err != nil {
				return fmt.Errorf("watermill router: %w", err)
			}
			return nil
		})

		g.Go(auctionSvc.ClosingWorker(runCtx))
		g.Go(billingSvc.ExpiryWorker(runCtx))

		return g.Wait()
	}

	return &Application{
		HTTPHandler:    rootRouter,
		RunWorkers:     runWorkers,
		BusRunning:     wmRouter.Running(),
		AuctionSvc:     auctionSvc,
		ParticipantSvc: participantSvc,
		BillingSvc:     billingSvc,
		SettlementSvc:  settlementSvc,
	}, nil
}

type contextMigrations struct {
	table string
	fs    fs.FS
}

func runMigrations(ctx context.Context, db *sql.DB, migrations []contextMigrations) error {
	for _, m := range migrations {
		provider, err := goose.NewProvider(goose.DialectPostgres, db, m.fs,
			goose.WithTableName(m.table))
		if err != nil {
			return fmt.Errorf("create goose provider (%s): %w", m.table, err)
		}
		if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("apply migrations (%s): %w", m.table, err)
		}
	}
	return nil
}

// NewDB wraps postgres.NewDB for use by the monolith command and tests.
func NewDB(ctx context.Context, dsn string) (*sql.DB, error) {
	return postgres.NewDB(ctx, dsn)
}
