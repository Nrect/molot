// Package server owns the HTTP edge of the monolith: the chi router
// with the middleware stack of ARCHITECTURE.md §8, healthz/readyz with
// injectable readiness probes, and graceful server startup/shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// NewRouter builds the root router with the §8 stack
// (RequestID → RealIP → otelhttp → slog request log → Recoverer → CORS
// → security headers) and returns it together with the /api subrouter,
// which additionally applies NoCache and the JWT auth middleware.
// Bounded contexts register their handlers on the api router; health
// endpoints and other unauthenticated routes go on the root.
func NewRouter(logger *slog.Logger, authMiddleware func(http.Handler) http.Handler) (root *chi.Mux, api chi.Router) {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// The §8 "RealIP" slot: chi deprecated middleware.RealIP as spoofable
	// (GHSA-3fxj-6jh8-hvhx); ClientIPFromRemoteAddr is its safe successor —
	// it records the peer address without trusting client-controlled
	// headers. Behind a trusted proxy, switch to
	// middleware.ClientIPFromXFFTrustedProxies(n).
	r.Use(middleware.ClientIPFromRemoteAddr)
	r.Use(otelhttp.NewMiddleware("molot.http"))
	r.Use(spanRouteNamer)
	r.Use(requestLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	r.Use(securityHeaders)

	api = r.Route("/api", func(api chi.Router) {
		api.Use(middleware.NoCache)
		api.Use(authMiddleware)
	})

	return r, api
}

// spanRouteNamer renames the otelhttp server span to
// "<METHOD> <chi route pattern>" once routing has resolved the pattern,
// so traces show "POST /api/auctions/{auctionID}/bids" instead of one
// generic operation name (§11) without exploding span-name cardinality.
func spanRouteNamer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
				trace.SpanFromContext(r.Context()).SetName(r.Method + " " + pattern)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLogger emits one slog record per served request; trace_id and
// correlation attributes come from the context handler in common/logs.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			defer func() {
				logger.LogAttrs(r.Context(), slog.LevelInfo, "http request served",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", ww.Status()),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", time.Since(start)),
					slog.String("request_id", middleware.GetReqID(r.Context())),
				)
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

// corsMiddleware is a deliberately small permissive CORS layer (the
// platform serves first-party UIs; chi has no bundled CORS middleware
// and a third-party dependency is not warranted for this policy).
// It reflects the Origin and short-circuits preflight requests.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "300")

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "deny")
		next.ServeHTTP(w, r)
	})
}

// ReadinessProbe is a named readiness check; readyz fails on the first
// failing probe and reports its name.
type ReadinessProbe struct {
	Name  string
	Check func(ctx context.Context) error
}

// RegisterHealthEndpoints mounts GET /healthz (liveness, always 200)
// and GET /readyz (503 on the first failing probe) on r — typically the
// root router, outside the authenticated /api group.
func RegisterHealthEndpoints(r chi.Router, logger *slog.Logger, readiness []ReadinessProbe) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		for _, probe := range readiness {
			if err := probe.Check(req.Context()); err != nil {
				logger.WarnContext(req.Context(), "readiness probe failed",
					slog.String("probe", probe.Name), slog.Any("error", err))
				http.Error(w, probe.Name+" not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

// RunHTTPServer serves handler on addr until ctx is cancelled, then
// shuts down gracefully with a 10s drain budget. It returns when the
// server has fully stopped.
func RunHTTPServer(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("http server: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}
		return <-serveErr
	}
}
