//go:build integration

package adapters_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/adapters"
	"molot/internal/billing/domain/invoice"
	billingevents "molot/internal/billing/events"
	"molot/internal/common/postgres"
	cwatermill "molot/internal/common/watermill"
)

var (
	pgOnce sync.Once
	pgDB   *sql.DB
	pgErr  error
)

// pgTestDB opens TEST_DATABASE_URL once per test binary, applies the
// billing migrations through a context-scoped goose version table and
// initializes the watermill topic schema the tx outbox publishes into.
func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set — skipping Postgres integration tests")
	}

	pgOnce.Do(func() {
		ctx := context.Background()
		pgDB, pgErr = postgres.NewDB(ctx, dsn)
		if pgErr != nil {
			return
		}

		provider, err := goose.NewProvider(goose.DialectPostgres, pgDB, adapters.Migrations(),
			goose.WithTableName("goose_db_version_billing"))
		if err != nil {
			pgErr = err
			return
		}
		if _, err := provider.Up(ctx); err != nil {
			pgErr = err
			return
		}

		// The tx publisher never auto-creates topic schema (it would
		// implicitly commit the business transaction); in production
		// subscribers initialize it at startup — mirror that here.
		subscriber, err := cwatermill.NewSQLSubscriber(pgDB, "billing-adapter-tests", 100*time.Millisecond, wm.NopLogger{})
		if err != nil {
			pgErr = err
			return
		}
		initializer, ok := subscriber.(interface{ SubscribeInitialize(topic string) error })
		if !ok {
			pgErr = errSubscriberInit
			return
		}
		pgErr = initializer.SubscribeInitialize(billingevents.Topic)
	})
	require.NoError(t, pgErr)
	return pgDB
}

var errSubscriberInit = &initError{}

type initError struct{}

func (*initError) Error() string { return "sql subscriber does not expose SubscribeInitialize" }

// TestInvoicePostgresRepository runs the same shared suite against the
// production Postgres adapter (gated by the integration build tag).
func TestInvoicePostgresRepository(t *testing.T) {
	t.Parallel()
	db := pgTestDB(t)
	testInvoiceRepository(t, func(t *testing.T) invoice.Repository {
		t.Helper()
		return adapters.NewInvoicePostgresRepository(db, slog.New(slog.DiscardHandler), wm.NopLogger{})
	})
}
