// Shared repository suite: ONE set of behavioral tests runs against
// every Repository implementation (BOOK_AUDIT rule 42) — in-memory here,
// Postgres in pg_repository_integration_test.go (build tag integration).
// Unique data per subtest instead of cleanup; t.Parallel() everywhere.
package adapters_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/participant/adapters"
	"molot/internal/participant/domain/participant"
)

type repositoryFactory func(t *testing.T) participant.Repository

func TestInMemoryRepository(t *testing.T) {
	t.Parallel()
	runRepositorySuite(t, func(t *testing.T) participant.Repository {
		return adapters.NewInMemoryRepository()
	})
}

func newRegistered(t *testing.T) *participant.Participant {
	t.Helper()
	id, err := participant.NewParticipantID(uuid.NewString())
	require.NoError(t, err)
	email, err := participant.NewEmailAddress(uuid.NewString() + "@example.com")
	require.NoError(t, err)
	p, err := participant.Register(id, email, "Suite Participant")
	require.NoError(t, err)
	return p
}

func runRepositorySuite(t *testing.T, newRepo repositoryFactory) {
	t.Helper()

	t.Run("add and get round-trip", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)

		require.NoError(t, repo.Add(t.Context(), p))

		got, err := repo.Get(t.Context(), p.ID())
		require.NoError(t, err)
		assert.Equal(t, p.ID(), got.ID())
		assert.Equal(t, p.Email(), got.Email())
		assert.Equal(t, p.DisplayName(), got.DisplayName())
		assert.Equal(t, participant.StatusRegistered, got.Status())
		assert.Equal(t, int64(1), got.Version())
		assert.Empty(t, got.PullDomainEvents(), "loaded aggregates carry no pending events")
	})

	t.Run("add with taken email returns ErrEmailTaken", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		first := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), first))

		otherID, err := participant.NewParticipantID(uuid.NewString())
		require.NoError(t, err)
		duplicate, err := participant.Register(otherID, first.Email(), "Copycat")
		require.NoError(t, err)

		require.ErrorIs(t, repo.Add(t.Context(), duplicate), participant.ErrEmailTaken)
	})

	t.Run("add with taken id returns ErrEmailTaken", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		first := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), first))

		otherEmail, err := participant.NewEmailAddress(uuid.NewString() + "@example.com")
		require.NoError(t, err)
		duplicate, err := participant.Register(first.ID(), otherEmail, "Copycat")
		require.NoError(t, err)

		require.ErrorIs(t, repo.Add(t.Context(), duplicate), participant.ErrEmailTaken)
	})

	t.Run("get missing returns NotFoundError", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		id, err := participant.NewParticipantID(uuid.NewString())
		require.NoError(t, err)

		_, err = repo.Get(t.Context(), id)

		var notFound participant.NotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.Equal(t, id, notFound.ID)
	})

	t.Run("update as operations persists the transition and bumps version", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), p))

		err := repo.UpdateAsOperations(t.Context(), p.ID(), verifyFn(time.Now()))
		require.NoError(t, err)

		got, err := repo.Get(t.Context(), p.ID())
		require.NoError(t, err)
		assert.True(t, got.IsVerified())
		assert.Equal(t, int64(2), got.Version(), "the adapter increments the optimistic-lock version")
	})

	t.Run("update of missing participant returns NotFoundError without calling updateFn", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		id, err := participant.NewParticipantID(uuid.NewString())
		require.NoError(t, err)

		called := false
		err = repo.UpdateAsOperations(t.Context(), id,
			func(_ context.Context, p *participant.Participant) (*participant.Participant, error) {
				called = true
				return p, nil
			})

		var notFound participant.NotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.False(t, called)
	})

	t.Run("updateFn error rolls the change back", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), p))

		sabotage := assert.AnError
		err := repo.UpdateAsOperations(t.Context(), p.ID(),
			func(_ context.Context, inTx *participant.Participant) (*participant.Participant, error) {
				require.NoError(t, inTx.Verify(time.Now())) // mutate, then fail: nothing may stick
				return nil, sabotage
			})
		require.ErrorIs(t, err, sabotage)

		got, err := repo.Get(t.Context(), p.ID())
		require.NoError(t, err)
		assert.False(t, got.IsVerified(), "rollback: the verify must not be persisted")
		assert.Equal(t, int64(1), got.Version())
	})

	t.Run("update enforces ownership inside the transaction", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), p))

		strangerID, err := participant.NewParticipantID(uuid.NewString())
		require.NoError(t, err)
		stranger, err := participant.NewActor(strangerID)
		require.NoError(t, err)

		called := false
		err = repo.Update(t.Context(), p.ID(), stranger,
			func(_ context.Context, inTx *participant.Participant) (*participant.Participant, error) {
				called = true
				return inTx, nil
			})

		var forbidden participant.ForbiddenParticipantUpdateError
		require.ErrorAs(t, err, &forbidden)
		assert.False(t, called, "updateFn must not run for a foreign actor")
	})

	t.Run("update as the owner succeeds", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), p))

		owner, err := participant.NewActor(p.ID())
		require.NoError(t, err)

		err = repo.Update(t.Context(), p.ID(), owner, verifyFn(time.Now()))
		require.NoError(t, err)

		got, err := repo.Get(t.Context(), p.ID())
		require.NoError(t, err)
		assert.True(t, got.IsVerified())
	})

	t.Run("race: concurrent verification has exactly one winner", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), p))

		const contenders = 20
		start := make(chan struct{})
		winners := make(chan struct{}, contenders)

		var wg sync.WaitGroup
		for range contenders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := repo.UpdateAsOperations(t.Context(), p.ID(), verifyFn(time.Now()))
				switch {
				case err == nil:
					winners <- struct{}{}
				default:
					assert.ErrorIs(t, err, participant.ErrAlreadyVerified)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(winners)

		assert.Len(t, drain(winners), 1, "exactly one transition must win")

		got, err := repo.Get(t.Context(), p.ID())
		require.NoError(t, err)
		assert.True(t, got.IsVerified())
		assert.Equal(t, int64(2), got.Version(), "losers must not bump the version")
	})
}

// verifyFn is the updateFn used by the happy-path mutation subtests.
func verifyFn(now time.Time) func(ctx context.Context, p *participant.Participant) (*participant.Participant, error) {
	return func(_ context.Context, p *participant.Participant) (*participant.Participant, error) {
		if err := p.Verify(now); err != nil {
			return nil, err
		}
		return p, nil
	}
}

func drain(ch chan struct{}) []struct{} {
	var out []struct{}
	for v := range ch {
		out = append(out, v)
	}
	return out
}
