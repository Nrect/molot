// Command monolith is the production entry point of the Molot modular
// monolith. All assembly lives in internal/monolith (the composition
// root shared with the component-test entry per BOOK_AUDIT rule 44);
// this file only owns the process concerns: config, signals, the HTTP
// listener and shutdown order.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"molot/internal/common/config"
	"molot/internal/common/logs"
	"molot/internal/common/server"
	"molot/internal/monolith"
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

	db, err := monolith.NewDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error("closing postgres pool", "error", closeErr)
		}
	}()

	app, shutdownObservability, err := monolith.NewApplication(ctx, cfg, logger, db)
	if err != nil {
		return err
	}
	defer func() {
		// Flush OTel providers with their own teardown budget — the run
		// ctx is already cancelled by the time deferred calls execute.
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownObservability(shutCtx)
	}()

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return app.RunWorkers(ctx) })

	g.Go(func() error {
		// Serve only after the bus is consuming, so readiness cannot
		// flap during startup.
		select {
		case <-app.BusRunning:
		case <-ctx.Done():
			return nil
		}
		addr := fmt.Sprintf(":%d", cfg.HTTPPort)
		logger.Info("http server listening", "addr", addr)
		return server.RunHTTPServer(ctx, addr, app.HTTPHandler)
	})

	return g.Wait()
}
