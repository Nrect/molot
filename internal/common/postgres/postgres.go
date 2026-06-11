// Package postgres owns the database/sql pool (pgx stdlib driver
// wrapped with otelsql tracing + pool metrics) and the transaction
// idiom of the codebase: RunInTx with a named error and deferred
// FinishTransaction (BOOK_AUDIT §4 item 18 — rollback errors are
// combined with the original error via multierr, never lost).
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/XSAM/otelsql"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.uber.org/multierr"

	// Registers the "pgx" database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Pool sizing. MaxIdle == MaxOpen on purpose: the bus subscribers poll
// continuously (BUS_POLL_INTERVAL), and the database/sql default of 2
// idle connections makes every poll open and close a fresh TCP
// connection — churn that exhausts ephemeral ports under load. Keeping
// every pooled connection reusable removes the churn entirely.
const (
	maxOpenConns    = 30
	connMaxIdleTime = 5 * time.Minute
)

// NewDB opens the process connection pool over the pgx stdlib driver,
// instrumented with otelsql (per-query spans + sql.DBStats pool
// metrics), and verifies connectivity with a ping bound to ctx.
func NewDB(ctx context.Context, dsn string) (*sql.DB, error) {
	attrs := otelsql.WithAttributes(semconv.DBSystemNamePostgreSQL)

	db, err := otelsql.Open("pgx", dsn, attrs)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	if _, err := otelsql.RegisterDBStatsMetrics(db, attrs); err != nil {
		err = fmt.Errorf("register db pool metrics: %w", err)
		return nil, multierr.Combine(err, db.Close())
	}

	if err := db.PingContext(ctx); err != nil {
		err = fmt.Errorf("ping postgres: %w", err)
		return nil, multierr.Combine(err, db.Close())
	}

	return db, nil
}

// RunInTx executes fn inside a transaction: commit when fn returns nil,
// rollback otherwise. The deferred FinishTransaction sees the named
// return error, so fn can simply return — no manual commit/rollback in
// call sites.
func RunInTx(ctx context.Context, db *sql.DB, fn func(ctx context.Context, tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() {
		err = FinishTransaction(err, tx)
	}()

	return fn(ctx, tx)
}

// FinishTransaction rolls back tx when err is non-nil (combining a
// failed rollback with the original error so neither is lost) and
// commits otherwise. Exported for adapters that manage their own
// BeginTx but must keep the same finishing idiom:
//
//	defer func() { err = postgres.FinishTransaction(err, tx) }()
func FinishTransaction(err error, tx *sql.Tx) error {
	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return multierr.Combine(err, fmt.Errorf("rollback tx: %w", rollbackErr))
		}
		return err
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("commit tx: %w", commitErr)
	}
	return nil
}
