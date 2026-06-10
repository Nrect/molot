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

	"molot/internal/common/auth"
	"molot/internal/common/config"
	"molot/internal/common/logs"
	"molot/internal/common/metrics"
	"molot/internal/common/postgres"
	"molot/internal/common/server"
	"molot/internal/common/tracing"
	cwatermill "molot/internal/common/watermill"
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
	// (embed.FS over internal/<ctx>/adapters/migrations) to this slice;
	// they run in order before anything serves.
	var migrations []fs.FS
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
	_ = subscriberConstructor // used by the context registration blocks below

	// --- http router --------------------------------------------------------
	rootRouter, apiRouter := server.NewRouter(logger, authMiddleware)
	_ = apiRouter // contexts mount their handlers here (RegisterHTTP)

	// --- context registration: auction ----------------------------------
	// auctionSvc := auctionservice.NewService(...)
	// auctionSvc.RegisterHTTP(apiRouter); auctionSvc.RegisterEventHandlers(wmRouter, subscriberConstructor)
	// g.Go(auctionSvc.ClosingWorker(ctx)) — joins the errgroup below.

	// --- context registration: participant -------------------------------
	// participantSvc := participantservice.NewService(...)
	// participantSvc.RegisterHTTP(apiRouter)

	// --- context registration: billing ------------------------------------
	// billingSvc := billingservice.NewService(...)
	// billingSvc.RegisterHTTP(apiRouter); g.Go(billingSvc.ExpiryWorker(ctx))

	// --- context registration: settlement ---------------------------------
	// settlementSvc := settlementservice.NewService(...)
	// settlementSvc.RegisterHTTP(apiRouter); settlementSvc.RegisterEventHandlers(wmRouter, subscriberConstructor)

	// --- context registration: notification -------------------------------
	// notificationSvc := notificationservice.NewService(...)
	// notificationSvc.RegisterEventHandlers(wmRouter, subscriberConstructor)

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

// runMigrations applies every context's embedded goose migrations.
// Per-context version tables arrive together with the first context
// FS (goose.WithTableName per context); with zero registered contexts
// this is a no-op.
func runMigrations(ctx context.Context, db *sql.DB, migrations []fs.FS) error {
	for _, fsys := range migrations {
		provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
		if err != nil {
			return fmt.Errorf("create goose provider: %w", err)
		}
		if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
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
