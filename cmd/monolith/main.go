// Command monolith is the composition root of the Molot modular
// monolith: config → logs → observability → auth → postgres →
// migrations → watermill router + outbox pub/sub → HTTP router →
// context services → errgroup{httpServer, watermillRouter}, all
// stopped gracefully on SIGINT/SIGTERM (ARCHITECTURE.md §9).
//
// Bounded contexts plug in at the explicitly marked registration
// blocks below; everything infrastructural is already wired.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pressly/goose/v3"
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

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	logger := logs.NewLogger(string(cfg.LogFormat))
	slog.SetDefault(logger)

	if err := run(cfg, logger); err != nil {
		logger.Error("monolith terminated", "error", err)
		os.Exit(1)
	}
	logger.Info("monolith stopped")
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- observability -------------------------------------------------
	tracerProvider, err := tracing.NewTracerProvider(ctx, cfg.OTELExporterOTLPEndpoint)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer shutdownGracefully(logger, "tracer provider", tracerProvider.Shutdown)

	meterProvider, err := metrics.NewMeterProvider(ctx, cfg.OTELExporterOTLPEndpoint)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdownGracefully(logger, "meter provider", meterProvider.Shutdown)

	// --- auth -----------------------------------------------------------
	// AUTH_MODE=jwks is a documented future: NewMiddleware fails fast
	// with actionable text instead of accepting tokens it cannot validate.
	authMiddleware, err := auth.NewMiddleware(string(cfg.AuthMode), cfg.AuthHS256Secret)
	if err != nil {
		return err
	}

	// --- postgres ---------------------------------------------------------
	// NewDB opens the otelsql-instrumented pgx pool and pings it with ctx.
	db, err := postgres.NewDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error("closing postgres pool", "error", closeErr)
		}
	}()

	// --- migrations -------------------------------------------------------
	// Each bounded context contributes its embedded goose migrations
	// (fs.FS rooted at the .sql files) with its own version table, so
	// contexts evolve their schemas independently; they all run before
	// anything serves.
	migrations := []contextMigrations{
		{table: "goose_db_version_participant", fs: participantservice.Migrations},
		{table: "goose_db_version_auction", fs: auctionservice.Migrations},
		{table: "goose_db_version_billing", fs: billingservice.Migrations()},
		{table: "goose_db_version_settlement", fs: settlementservice.Migrations},
		{table: "goose_db_version_notification", fs: notificationservice.Migrations},
	}
	if err := runMigrations(ctx, db, migrations); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// --- watermill --------------------------------------------------------
	wmLogger := cwatermill.NewLogger(logger)

	deadLetterPublisher, err := cwatermill.NewSQLPublisher(db, wmLogger)
	if err != nil {
		return fmt.Errorf("create dead letter publisher: %w", err)
	}

	wmRouter, err := cwatermill.NewRouter(wmLogger, deadLetterPublisher)
	if err != nil {
		return fmt.Errorf("create watermill router: %w", err)
	}

	// Shared subscriber constructor for every context's event processor:
	// one consumer group (= offset cursor) per handler name.
	subscriberConstructor := func(handlerName string) (message.Subscriber, error) {
		return cwatermill.NewSQLSubscriber(db, handlerName, wmLogger)
	}

	// Bus health gauges (§11): dead-letter size and per-topic consumer
	// lag, observed by scanning the watermill tables on every metric
	// collection.
	if err := cwatermill.RegisterBusMetrics(db, meterProvider); err != nil {
		return fmt.Errorf("register bus metrics: %w", err)
	}

	// --- http router --------------------------------------------------------
	rootRouter, apiRouter := server.NewRouter(logger, authMiddleware)

	// --- context registration: auction ----------------------------------
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
		return fmt.Errorf("assemble auction context: %w", err)
	}
	auctionSvc.RegisterHTTP(apiRouter)
	if err := auctionSvc.RegisterEventHandlers(wmRouter, subscriberConstructor); err != nil {
		return fmt.Errorf("register auction event handlers: %w", err)
	}

	// --- context registration: participant -------------------------------
	participantSvc, err := participantservice.NewService(db, logger, meterProvider, tracerProvider)
	if err != nil {
		return fmt.Errorf("assemble participant context: %w", err)
	}
	participantSvc.RegisterHTTP(apiRouter)
	// Registration IS the entry (§8): POST /participants lives on the
	// root router, outside the JWT-protected /api group.
	participantSvc.RegisterPublicHTTP(rootRouter)

	// --- context registration: billing ------------------------------------
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
		return fmt.Errorf("assemble billing context: %w", err)
	}
	billingSvc.RegisterHTTP(apiRouter)

	// --- context registration: settlement ---------------------------------
	// The saga consumes the other contexts' sync facades; the interface
	// types live in settlement/adapters, the values arrive from here
	// (ADR-0004).
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
		return fmt.Errorf("assemble settlement context: %w", err)
	}
	settlementSvc.RegisterHTTP(apiRouter)
	if err := settlementSvc.RegisterEventHandlers(wmRouter, subscriberConstructor); err != nil {
		return fmt.Errorf("register settlement event handlers: %w", err)
	}

	// --- context registration: notification -------------------------------
	notificationSvc := notificationservice.NewService(db, logger)
	if err := notificationSvc.RegisterEventHandlers(wmRouter, subscriberConstructor); err != nil {
		return fmt.Errorf("register notification event handlers: %w", err)
	}

	// --- health -----------------------------------------------------------
	server.RegisterHealthEndpoints(rootRouter, logger, []server.ReadinessProbe{
		{Name: "postgres", Check: db.PingContext},
		{Name: "watermill-router", Check: func(context.Context) error {
			if !wmRouter.IsRunning() {
				return errors.New("router is not running")
			}
			return nil
		}},
	})

	// --- run --------------------------------------------------------------
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if err := wmRouter.Run(ctx); err != nil {
			return fmt.Errorf("watermill router: %w", err)
		}
		return nil
	})

	// The two in-binary workers (§10): by-time auction closing and
	// invoice payment timeout. They stop with everyone else on ctx.
	g.Go(auctionSvc.ClosingWorker(ctx))
	g.Go(billingSvc.ExpiryWorker(ctx))

	g.Go(func() error {
		// Serve only after the bus is consuming, so readiness cannot
		// flap during startup.
		select {
		case <-wmRouter.Running():
		case <-ctx.Done():
			return nil
		}
		addr := fmt.Sprintf(":%d", cfg.HTTPPort)
		logger.Info("http server listening", "addr", addr)
		return server.RunHTTPServer(ctx, addr, rootRouter)
	})

	return g.Wait()
}

// contextMigrations is one bounded context's goose migration set with
// its dedicated version table (goose_db_version_<ctx>).
type contextMigrations struct {
	table string
	fs    fs.FS
}

// runMigrations applies every context's embedded goose migrations in
// order, each tracked in its own version table so contexts version
// their schemas independently.
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

// shutdownGracefully flushes an observability provider with its own
// teardown budget (the run ctx is already cancelled at this point).
func shutdownGracefully(logger *slog.Logger, name string, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("shutting down "+name, "error", err)
	}
}
