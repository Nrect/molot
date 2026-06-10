// Command monolith is the composition root of the Molot modular
// monolith: config -> logs -> observability -> postgres -> migrations
// -> watermill router -> context services -> HTTP server, all stopped
// gracefully via one errgroup on SIGINT/SIGTERM.
//
// Infrastructure blocks that depend on not-yet-pinned modules (OTel,
// pgx/otelsql, goose, watermill, chi) are marked below and wired by the
// integration agent once go.mod carries the dependencies; the wiring
// order and ownership stay exactly as documented in ARCHITECTURE.md §9.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"molot/internal/common/config"
	"molot/internal/common/logs"
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

	// AUTH_MODE=jwks is a documented future. Fail fast with actionable
	// text instead of accepting tokens we cannot validate. This check
	// moves into the common/auth middleware constructor once the JWT
	// dependency lands.
	if cfg.AuthMode == config.AuthModeJWKS {
		return errors.New("AUTH_MODE=jwks is not implemented yet: run with AUTH_MODE=local-hs256 and AUTH_HS256_SECRET (JWKS validation is a planned future)")
	}

	// --- observability ------------------------------------------------
	// OTel tracing + metrics (OTLP gRPC to cfg.OTELExporterOTLPEndpoint,
	// resource service.name=molot, runtime instrumentation) initialize
	// here; their Shutdown joins the errgroup teardown.

	// --- postgres -----------------------------------------------------
	// db := postgres.NewPool(ctx, cfg.DatabaseURL) — pgx stdlib driver
	// wrapped with otelsql; closed on shutdown.

	// --- migrations ---------------------------------------------------
	// goose embedded migrations run here, per context schema
	// (internal/<ctx>/adapters/migrations), before anything serves.

	// --- watermill ----------------------------------------------------
	// One message.Router per binary (CorrelationID -> PoisonQueue ->
	// Retry -> Recoverer), SQL publisher/subscriber over db
	// (transactional outbox). router.Run joins the errgroup below and
	// readiness gates on router.Running().

	// --- context registration (filled by the integration agent) -------
	// auctionSvc := auctionservice.NewService(...)        // RegisterHTTP, RegisterEventHandlers, ClosingWorker
	// participantSvc := participantservice.NewService(...) // RegisterHTTP
	// billingSvc := billingservice.NewService(...)         // RegisterHTTP, ExpiryWorker
	// settlementSvc := settlementservice.NewService(...)   // RegisterHTTP, RegisterEventHandlers
	// notificationSvc := notificationservice.NewService(...) // RegisterEventHandlers

	// Readiness probes are injected as functions; DB ping and
	// router.Running() append here as the infrastructure above lands.
	var readiness []probe

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:           healthRoutes(readiness),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-ctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}
		return nil
	})

	return g.Wait()
}

// probe is a named readiness check; readyz fails on the first failing one.
type probe struct {
	name  string
	check func(ctx context.Context) error
}

func healthRoutes(readiness []probe) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range readiness {
			if err := p.check(r.Context()); err != nil {
				slog.WarnContext(r.Context(), "readiness probe failed", "probe", p.name, "error", err)
				http.Error(w, p.name+" not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	return mux
}
