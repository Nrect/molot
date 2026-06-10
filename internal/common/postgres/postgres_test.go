package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/postgres"
)

// --- minimal database/sql fake (no external driver, no network) ----------
//
// Just enough driver surface for BeginTx/Commit/Rollback bookkeeping;
// sabotage of commit/rollback exercises the FinishTransaction branches
// (BOOK_AUDIT §4: a failed rollback must not swallow the original error).

type fakeConn struct {
	beginErr    error
	commitErr   error
	rollbackErr error

	begins    int
	commits   int
	rollbacks int
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (c *fakeConn) Close() error                        { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	c.begins++
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return &fakeTx{conn: c}, nil
}

type fakeTx struct{ conn *fakeConn }

func (tx *fakeTx) Commit() error {
	tx.conn.commits++
	return tx.conn.commitErr
}

func (tx *fakeTx) Rollback() error {
	tx.conn.rollbacks++
	return tx.conn.rollbackErr
}

type fakeConnector struct{ conn *fakeConn }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

func newFakeDB(t *testing.T, conn *fakeConn) *sql.DB {
	t.Helper()

	db := sql.OpenDB(fakeConnector{conn: conn})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- RunInTx --------------------------------------------------------------

func TestRunInTx(t *testing.T) {
	t.Parallel()

	fnErr := errors.New("business rule failed")
	beginErr := errors.New("pool exhausted")
	commitErr := errors.New("serialization failure")
	rollbackErr := errors.New("connection lost during rollback")

	testCases := []struct {
		name string
		conn *fakeConn
		fn   func(ctx context.Context, tx *sql.Tx) error

		wantErrs      []error
		wantCommits   int
		wantRollbacks int
	}{
		{
			name:        "success commits",
			conn:        &fakeConn{},
			fn:          func(context.Context, *sql.Tx) error { return nil },
			wantCommits: 1,
		},
		{
			name:          "fn error rolls back and is returned",
			conn:          &fakeConn{},
			fn:            func(context.Context, *sql.Tx) error { return fnErr },
			wantErrs:      []error{fnErr},
			wantRollbacks: 1,
		},
		{
			name:          "failed rollback keeps both errors",
			conn:          &fakeConn{rollbackErr: rollbackErr},
			fn:            func(context.Context, *sql.Tx) error { return fnErr },
			wantErrs:      []error{fnErr, rollbackErr},
			wantRollbacks: 1,
		},
		{
			name:        "failed commit is surfaced",
			conn:        &fakeConn{commitErr: commitErr},
			fn:          func(context.Context, *sql.Tx) error { return nil },
			wantErrs:    []error{commitErr},
			wantCommits: 1,
		},
		{
			name:     "begin error short-circuits",
			conn:     &fakeConn{beginErr: beginErr},
			fn:       func(context.Context, *sql.Tx) error { t.Fatal("fn must not run"); return nil },
			wantErrs: []error{beginErr},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := newFakeDB(t, tc.conn)

			err := postgres.RunInTx(context.Background(), db, tc.fn)

			if len(tc.wantErrs) == 0 {
				require.NoError(t, err)
			}
			for _, wantErr := range tc.wantErrs {
				assert.ErrorIs(t, err, wantErr)
			}
			assert.Equal(t, tc.wantCommits, tc.conn.commits, "commit calls")
			assert.Equal(t, tc.wantRollbacks, tc.conn.rollbacks, "rollback calls")
		})
	}
}

func TestRunInTxPassesTheOpenTx(t *testing.T) {
	t.Parallel()

	conn := &fakeConn{}
	db := newFakeDB(t, conn)

	var gotTx *sql.Tx
	err := postgres.RunInTx(context.Background(), db, func(_ context.Context, tx *sql.Tx) error {
		gotTx = tx
		return nil
	})

	require.NoError(t, err)
	assert.NotNil(t, gotTx)
	assert.Equal(t, 1, conn.begins)
}

// --- FinishTransaction (exported idiom for adapters) ----------------------

func TestFinishTransaction(t *testing.T) {
	t.Parallel()

	fnErr := errors.New("write failed")

	t.Run("nil error commits", func(t *testing.T) {
		t.Parallel()

		conn := &fakeConn{}
		db := newFakeDB(t, conn)
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)

		require.NoError(t, postgres.FinishTransaction(nil, tx))
		assert.Equal(t, 1, conn.commits)
		assert.Equal(t, 0, conn.rollbacks)
	})

	t.Run("error rolls back", func(t *testing.T) {
		t.Parallel()

		conn := &fakeConn{}
		db := newFakeDB(t, conn)
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)

		got := postgres.FinishTransaction(fnErr, tx)
		assert.ErrorIs(t, got, fnErr)
		assert.Equal(t, 0, conn.commits)
		assert.Equal(t, 1, conn.rollbacks)
	})
}
