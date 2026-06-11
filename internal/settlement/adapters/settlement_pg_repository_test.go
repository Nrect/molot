//go:build integration

package adapters_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"molot/internal/common/postgres"
	"molot/internal/settlement/adapters"
	"molot/internal/settlement/domain/settlement"
)

var (
	pgOnce sync.Once
	pgDB   *sql.DB
	pgErr  error
)

// pgTestDB opens TEST_DATABASE_URL once per test binary and applies the
// settlement migrations through a context-scoped goose version table.
// Settlement publishes no integration events, so no watermill topic
// schema is needed here.
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
		provider, err := goose.NewProvider(goose.DialectPostgres, pgDB, adapters.MigrationsForTests(),
			goose.WithTableName("goose_db_version_settlement"))
		if err != nil {
			pgErr = err
			return
		}
		if _, err := provider.Up(ctx); err != nil {
			pgErr = err
		}
	})
	require.NoError(t, pgErr)
	return pgDB
}

// TestSettlementPostgresRepository runs the same shared suite against
// the production Postgres adapter (gated by the integration build tag).
func TestSettlementPostgresRepository(t *testing.T) {
	t.Parallel()
	db := pgTestDB(t)
	testSettlementRepository(t, func(t *testing.T) settlement.Repository {
		t.Helper()
		return adapters.NewSettlementPostgresRepository(db, noop.NewMeterProvider())
	})
}

// TestSettlementPostgresReadModel covers the direct-read view (§5) the
// operations query is served from.
func TestSettlementPostgresReadModel(t *testing.T) {
	t.Parallel()
	db := pgTestDB(t)
	repo := adapters.NewSettlementPostgresRepository(db, noop.NewMeterProvider())
	ctx := context.Background()

	t.Run("returns the saga view", func(t *testing.T) {
		t.Parallel()
		s := startedFixture(t, sagaOpts{qualifies: true})
		require.NoError(t, repo.Add(ctx, s))
		require.NoError(t, repo.Update(ctx, s.AuctionID(),
			func(_ context.Context, current *settlement.Settlement) (*settlement.Settlement, error) {
				if err := current.InvoiceIssued(current.InvoiceID()); err != nil {
					return nil, err
				}
				return current, nil
			}))

		view, err := repo.SettlementByAuction(ctx, s.AuctionID())
		require.NoError(t, err)
		assert.Equal(t, s.AuctionID().String(), view.AuctionID)
		assert.Equal(t, "awaiting_payment", view.State)
		assert.Equal(t, s.Winner().String(), view.WinnerID)
		assert.Equal(t, s.Hammer().Amount(), view.HammerMinor)
		assert.Equal(t, s.RunnerUp().String(), view.RunnerUpID)
		assert.True(t, view.RunnerUpQualifies)
		assert.Equal(t, 1, view.Attempt)
		assert.Equal(t, s.InvoiceID().String(), view.InvoiceID)
		assert.Empty(t, view.FailureReason)
	})

	t.Run("missing saga is not found", func(t *testing.T) {
		t.Parallel()
		s := startedFixture(t, sagaOpts{})
		_, err := repo.SettlementByAuction(ctx, s.AuctionID())
		var notFound settlement.NotFoundError
		assert.ErrorAs(t, err, &notFound)
	})
}
