//go:build component

// Component tests (BOOK_AUDIT rule 44, ARCHITECTURE §12).
//
// Setup:
//
//	docker compose up -d postgres
//	COMPONENT_DATABASE_URL=postgres://molot:molot@localhost:5432/molot_component?sslmode=disable \
//	  go test -race -tags component ./tests/...
//
// COMPONENT_DATABASE_URL is an ADMIN entry point: every application instance
// (the shared one assembled here and the per-test ones from buildApp) gets its
// own freshly created database, dropped on teardown. Isolation is total by
// construction: no dirty state between runs, and no cross-instance consumer
// groups competing for one bus with different configs.
//
// The package-level sharedClient / sharedSrv are set up once by TestMain
// and shared across all sub-tests. Tests that need a non-default PSP mode
// (decline/flaky) or payment term assemble their own Application; see
// settlement_compensation_test.go and payment_refund_test.go for the pattern.
package component_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	os.Exit(testMain(m))
}

// testMain exists so deferred teardown actually runs — os.Exit directly in
// TestMain would skip every defer.
func testMain(m *testing.M) int {
	adminDSN := os.Getenv("COMPONENT_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr,
			"COMPONENT_DATABASE_URL not set; skipping component tests")
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, cleanupDB, err := createIsolatedDB(ctx, adminDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create shared component DB: %v\n", err)
		return 1
	}
	defer cleanupDB()

	app, err := monolith.NewComponentTestApplication(
		ctx, db, testHS256Secret, testPlatformCurrency,
		testDefaultPaymentTerm, testDefaultPSPMode,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "assemble component test application: %v\n", err)
		return 1
	}
	sharedApp = app

	// Start background workers (watermill router + closing/expiry workers)
	// for the lifetime of the test process.
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		if wErr := app.RunWorkers(ctx); wErr != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "RunWorkers: %v\n", wErr)
		}
	}()

	// Ordered teardown (this defer runs BEFORE the cleanupDB defer above):
	// cancel → wait for RunWorkers to exit → only then close the pool and
	// drop the database. Without the wait, subscribers keep polling a
	// closed pool and spam "sql: database is closed" errors on shutdown.
	defer func() {
		cancel()
		select {
		case <-workersDone:
		case <-time.After(10 * time.Second):
			fmt.Fprintln(os.Stderr, "RunWorkers did not exit within 10 s; dropping DB anyway")
		}
	}()

	// Serve HTTP so the port is available immediately (no "start listening"
	// race). httptest.NewServer uses a random free port.
	sharedSrv = httptest.NewServer(app.HTTPHandler)
	defer sharedSrv.Close()

	sharedClient = tests.NewClient(sharedSrv.URL, testHS256Secret)

	// Poll /readyz until the watermill router is running (max 15 s).
	if !waitForReadyz(sharedSrv.URL+"/readyz", 15*time.Second) {
		fmt.Fprintln(os.Stderr, "readyz did not return 200 within 15s")
		return 1
	}

	return m.Run()
}

// createIsolatedDB creates a uniquely named database on the Postgres server
// behind adminDSN and opens a pool to it. The returned cleanup closes the
// pool and drops the database (FORCE terminates lingering connections).
func createIsolatedDB(ctx context.Context, adminDSN string) (*sql.DB, func(), error) {
	admin, err := monolith.NewDB(ctx, adminDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("connect admin db: %w", err)
	}

	dbName := "molot_comp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("create database %s: %w", dbName, err)
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("parse admin DSN: %w", err)
	}
	u.Path = "/" + dbName

	db, err := monolith.NewDB(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		admin.Close()
		return nil, nil, fmt.Errorf("open database %s: %w", dbName, err)
	}

	cleanup := func() {
		db.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		admin.Close()
	}
	return db, cleanup, nil
}

// waitForReadyz polls the readyz endpoint until it returns 200 or the
// deadline passes. No sleep — tight poll to minimise test start time.
func waitForReadyz(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
