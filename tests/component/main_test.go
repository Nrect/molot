//go:build component

// Component tests (BOOK_AUDIT rule 44, ARCHITECTURE §12).
//
// Setup:
//   docker compose up -d postgres
//   psql "postgres://molot:molot@localhost:5432/postgres?sslmode=disable" \
//        -c "CREATE DATABASE molot_component;" 2>/dev/null || true
//   COMPONENT_DATABASE_URL=postgres://molot:molot@localhost:5432/molot_component?sslmode=disable \
//     go test -race -tags component ./tests/...
//
// A dedicated molot_component DB avoids conflicts with the integration DB
// (molot). Migrations run once in TestMain; the DB is NOT cleaned between
// tests — each test uses fresh UUIDs so there are no collisions (rule 42).
//
// The package-level sharedClient / sharedSrv are set up once by TestMain
// and shared across all sub-tests. Tests that need a non-default PSP mode
// (decline/flaky) must assemble their own Application; see
// settlement_compensation_test.go and payment_refund_test.go for the
// pattern.
package component_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	billingadapters "molot/internal/billing/adapters"
	"molot/internal/monolith"
	"molot/tests"
)

const (
	testHS256Secret        = "component-test-secret-at-least-32-bytes!!"
	testPlatformCurrency   = "EUR"
	testDefaultPaymentTerm = 5 * time.Second
	testDefaultPSPMode     = billingadapters.PSPModeSuccess
)

// Package-level test fixtures, set by TestMain.
var (
	sharedSrv    *httptest.Server
	sharedClient *tests.Client
	sharedApp    *monolith.Application
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("COMPONENT_DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr,
			"COMPONENT_DATABASE_URL not set; skipping component tests")
		os.Exit(0)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := monolith.NewDB(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect component DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	app, err := monolith.NewComponentTestApplication(
		ctx, db, testHS256Secret, testPlatformCurrency,
		testDefaultPaymentTerm, testDefaultPSPMode,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "assemble component test application: %v\n", err)
		os.Exit(1)
	}
	sharedApp = app

	// Start background workers (watermill router + closing/expiry workers)
	// for the lifetime of the test process.
	go func() {
		if wErr := app.RunWorkers(ctx); wErr != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "RunWorkers: %v\n", wErr)
		}
	}()

	// Serve HTTP so the port is available immediately (no "start listening"
	// race). httptest.NewServer uses a random free port.
	sharedSrv = httptest.NewServer(app.HTTPHandler)
	defer sharedSrv.Close()

	sharedClient = tests.NewClient(sharedSrv.URL, testHS256Secret)

	// Poll /readyz until the watermill router is running (max 15 s).
	waitForReadyz(sharedSrv.URL+"/readyz", 15*time.Second)

	os.Exit(m.Run())
}

// waitForReadyz polls the readyz endpoint until it returns 200 or the
// deadline passes. No sleep — tight poll to minimise test start time.
func waitForReadyz(url string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "readyz did not return 200 within %s\n", timeout)
	os.Exit(1)
}
