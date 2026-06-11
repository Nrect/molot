// One shared behavioural suite per store interface, run against every
// implementation (BOOK_AUDIT rule 42): the in-memory ones here, the
// Postgres ones in pg_integration_test.go (build tag `integration`,
// skipped without TEST_DATABASE_URL — unit runs stay Docker-free).
//
// No rollback test on purpose: the context has no updateFn/transaction
// flows — its only write paths are single-statement idempotent
// INSERTs/UPSERTs (see the context README).
package adapters_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/notification/adapters"
)

// Local mirrors of the ports interfaces: adapters must not depend on
// ports, and the suite only needs the behavioural contract.
type recipientDirectory interface {
	UpsertRecipient(ctx context.Context, participantID, email, displayName string) error
	RecipientByID(ctx context.Context, participantID string) (email, displayName string, err error)
}

type sellerDirectory interface {
	RememberSeller(ctx context.Context, auctionID, sellerID string) error
	SellerOf(ctx context.Context, auctionID string) (string, error)
}

type sentLog interface {
	FirstDelivery(ctx context.Context, eventID, kind, recipientID string) (bool, error)
}

// --- suites -----------------------------------------------------------------

func runRecipientsSuite(t *testing.T, newStore func(t *testing.T) recipientDirectory) {
	t.Run("upsert then lookup", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		id := uuid.NewString()

		require.NoError(t, store.UpsertRecipient(t.Context(), id, "alice@example.com", "Alice"))

		email, displayName, err := store.RecipientByID(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, "alice@example.com", email)
		assert.Equal(t, "Alice", displayName)
	})

	t.Run("redelivered upsert overwrites with the latest data", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		id := uuid.NewString()

		require.NoError(t, store.UpsertRecipient(t.Context(), id, "old@example.com", "Old Name"))
		require.NoError(t, store.UpsertRecipient(t.Context(), id, "new@example.com", "New Name"))

		email, displayName, err := store.RecipientByID(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, "new@example.com", email)
		assert.Equal(t, "New Name", displayName)
	})

	t.Run("unknown recipient errors", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		_, _, err := store.RecipientByID(t.Context(), uuid.NewString())
		require.Error(t, err, "projection lag must surface as an error for bus redelivery")
	})
}

func runSellersSuite(t *testing.T, newStore func(t *testing.T) sellerDirectory) {
	t.Run("remember then resolve", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		auctionID, sellerID := uuid.NewString(), uuid.NewString()

		require.NoError(t, store.RememberSeller(t.Context(), auctionID, sellerID))

		got, err := store.SellerOf(t.Context(), auctionID)
		require.NoError(t, err)
		assert.Equal(t, sellerID, got)
	})

	t.Run("remember is idempotent on redelivery", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		auctionID, sellerID := uuid.NewString(), uuid.NewString()

		require.NoError(t, store.RememberSeller(t.Context(), auctionID, sellerID))
		require.NoError(t, store.RememberSeller(t.Context(), auctionID, sellerID))

		got, err := store.SellerOf(t.Context(), auctionID)
		require.NoError(t, err)
		assert.Equal(t, sellerID, got)
	})

	t.Run("unknown auction errors", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		_, err := store.SellerOf(t.Context(), uuid.NewString())
		require.Error(t, err)
	})
}

func runSentLogSuite(t *testing.T, newLog func(t *testing.T) sentLog) {
	t.Run("second delivery of the same slot is suppressed", func(t *testing.T) {
		t.Parallel()
		log := newLog(t)
		eventID, recipientID := uuid.NewString(), uuid.NewString()

		first, err := log.FirstDelivery(t.Context(), eventID, "some_kind", recipientID)
		require.NoError(t, err)
		assert.True(t, first)

		again, err := log.FirstDelivery(t.Context(), eventID, "some_kind", recipientID)
		require.NoError(t, err)
		assert.False(t, again, "redelivery must not send twice")
	})

	t.Run("one event fans out to several recipients", func(t *testing.T) {
		t.Parallel()
		log := newLog(t)
		eventID := uuid.NewString()
		winner, seller := uuid.NewString(), uuid.NewString()

		first, err := log.FirstDelivery(t.Context(), eventID, "some_kind", winner)
		require.NoError(t, err)
		assert.True(t, first)

		second, err := log.FirstDelivery(t.Context(), eventID, "some_kind", seller)
		require.NoError(t, err)
		assert.True(t, second, "a different recipient of the same event owns its own slot")
	})

	t.Run("one event fans out to several kinds", func(t *testing.T) {
		t.Parallel()
		log := newLog(t)
		eventID, recipientID := uuid.NewString(), uuid.NewString()

		first, err := log.FirstDelivery(t.Context(), eventID, "kind_a", recipientID)
		require.NoError(t, err)
		assert.True(t, first)

		second, err := log.FirstDelivery(t.Context(), eventID, "kind_b", recipientID)
		require.NoError(t, err)
		assert.True(t, second, "a different kind for the same event owns its own slot")
	})

	t.Run("exactly one winner under concurrent redelivery", func(t *testing.T) {
		t.Parallel()
		log := newLog(t)
		eventID, recipientID := uuid.NewString(), uuid.NewString()

		const workers = 20
		start := make(chan struct{})
		firsts := make(chan bool, workers)
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				first, err := log.FirstDelivery(context.Background(), eventID, "race_kind", recipientID)
				errs <- err
				firsts <- first
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		close(firsts)

		for err := range errs {
			require.NoError(t, err)
		}
		winners := 0
		for first := range firsts {
			if first {
				winners++
			}
		}
		assert.Equal(t, 1, winners, "exactly one concurrent delivery may win the slot")
	})
}

// --- in-memory runs ----------------------------------------------------------

func TestRecipientsInMem(t *testing.T) {
	t.Parallel()
	runRecipientsSuite(t, func(*testing.T) recipientDirectory { return adapters.NewRecipientsInMem() })
}

func TestSellersInMem(t *testing.T) {
	t.Parallel()
	runSellersSuite(t, func(*testing.T) sellerDirectory { return adapters.NewSellersInMem() })
}

func TestSentLogInMem(t *testing.T) {
	t.Parallel()
	runSentLogSuite(t, func(*testing.T) sentLog { return adapters.NewSentLogInMem() })
}
