//go:build integration

// Postgres half of the shared adapter suite. Needs a running Postgres
// (docker compose up -d postgres) and TEST_DATABASE_URL; skips itself
// otherwise. Test data is unique per run (uuid), no cleanup (BOOK_AUDIT
// rule 42).
package adapters_test

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	// Registers the "pgx" database/sql driver for sql.Open below.
	_ "github.com/jackc/pgx/v5/stdlib"

	"molot/internal/notification/adapters"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

// testDB opens a pool on TEST_DATABASE_URL and applies the context's
// migrations exactly once per test process, tracked in the context's
// own goose version table.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set — skipping Postgres integration suite")
	}

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	migrateOnce.Do(func() { migrateErr = applyMigrations(db) })
	require.NoError(t, migrateErr, "apply notification migrations")
	return db
}

func applyMigrations(db *sql.DB) error {
	fsys, err := fs.Sub(adapters.Migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName("goose_db_version_notification"))
	if err != nil {
		return err
	}
	_, err = provider.Up(context.Background())
	return err
}

func TestRecipientsPG(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	runRecipientsSuite(t, func(*testing.T) recipientDirectory { return adapters.NewRecipientsPG(db) })
}

func TestSellersPG(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	runSellersSuite(t, func(*testing.T) sellerDirectory { return adapters.NewSellersPG(db) })
}

func TestSentLogPG(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	runSentLogSuite(t, func(*testing.T) sentLog { return adapters.NewSentLogPG(db) })
}
