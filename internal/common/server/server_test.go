package server_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/server"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// passthroughAuth marks requests it saw, standing in for the JWT
// middleware (auth has its own tests).
func passthroughAuth(sawAuth *bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*sawAuth = true
			next.ServeHTTP(w, r)
		})
	}
}

func TestNewRouterStack(t *testing.T) {
	t.Parallel()

	var sawAuth bool
	root, api := server.NewRouter(discardLogger(), passthroughAuth(&sawAuth))
	api.Get("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	root.Get("/public", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("api routes pass auth, NoCache and security headers", func(t *testing.T) {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ping", nil))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, sawAuth, "auth middleware must guard /api")
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		assert.Equal(t, "deny", rec.Header().Get("X-Frame-Options"))
		assert.Contains(t, rec.Header().Get("Cache-Control"), "no-cache")
	})

	t.Run("routes outside /api skip auth", func(t *testing.T) {
		sawAuth = false
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public", nil))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, sawAuth, "auth middleware must not guard public routes")
	})

	t.Run("CORS preflight is short-circuited", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/ping", nil)
		req.Header.Set("Origin", "https://ui.example")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, "https://ui.example", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.NotEmpty(t, rec.Header().Get("Access-Control-Allow-Methods"))
	})

	t.Run("panic in handler is recovered", func(t *testing.T) {
		root.Get("/boom", func(http.ResponseWriter, *http.Request) {
			panic("kaboom")
		})

		rec := httptest.NewRecorder()
		require.NotPanics(t, func() {
			root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("pool exhausted")

	testCases := []struct {
		name       string
		probes     []server.ReadinessProbe
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no probes is ready",
			probes:     nil,
			wantStatus: http.StatusOK,
		},
		{
			name: "all probes pass",
			probes: []server.ReadinessProbe{
				{Name: "postgres", Check: func(context.Context) error { return nil }},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "failing probe returns 503 with its name",
			probes: []server.ReadinessProbe{
				{Name: "postgres", Check: func(context.Context) error { return nil }},
				{Name: "watermill-router", Check: func(context.Context) error { return probeErr }},
			},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "watermill-router not ready",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root, _ := server.NewRouter(discardLogger(), passthroughAuth(new(bool)))
			server.RegisterHealthEndpoints(root, discardLogger(), tc.probes)

			rec := httptest.NewRecorder()
			root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			assert.Equal(t, http.StatusOK, rec.Code, "healthz is liveness only")

			rec = httptest.NewRecorder()
			root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantBody != "" {
				assert.Contains(t, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestRunHTTPServerGracefulShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- server.RunHTTPServer(ctx, "127.0.0.1:0", http.NewServeMux())
	}()

	cancel()
	require.NoError(t, <-done)
}
