//go:build integration

// Postgres half of the shared repository suite. Runs only with the
// integration build tag and a TEST_DATABASE_URL (make test-integration
// brings up the compose postgres); skips itself otherwise so the unit
// run never needs Docker.
package adapters_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/postgres"
	cwatermill "molot/internal/common/watermill"
	"molot/internal/participant/adapters"
	"molot/internal/participant/domain/participant"
	"molot/internal/participant/events"
)

func TestPostgresRepository(t *testing.T) {
	t.Parallel()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; run via `make test-integration`")
	}

	db, err := postgres.NewDB(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Per-context goose version table, as the monolith does per context.
	store, err := database.NewStore(database.DialectPostgres, "goose_db_version_participant")
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectCustom, db, adapters.Migrations, goose.WithStore(store))
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)

	wmLogger := cwatermill.NewLogger(nil)
	require.NoError(t, adapters.InitializeEventsSchema(db, wmLogger),
		"watermill topic schema must exist for the outbox publisher")

	newRepo := func(t *testing.T) participant.Repository {
		return adapters.NewPostgresRepository(db, wmLogger)
	}

	runRepositorySuite(t, newRepo)

	t.Run("outbox: integration events commit with the aggregate", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		id := p.ID().String()

		require.NoError(t, repo.Add(t.Context(), p))
		require.NoError(t, repo.UpdateAsOperations(t.Context(), p.ID(), verifyFn(time.Now())))

		assert.Equal(t, 1, countOutboxEvents(t, db, "ParticipantRegisteredV1", id))
		assert.Equal(t, 1, countOutboxEvents(t, db, "ParticipantVerifiedV1", id))
	})

	t.Run("outbox: rolled back updateFn publishes nothing", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		p := newRegistered(t)
		id := p.ID().String()
		require.NoError(t, repo.Add(t.Context(), p))

		err := repo.UpdateAsOperations(t.Context(), p.ID(), failAfterVerify())
		require.ErrorIs(t, err, assert.AnError)

		assert.Zero(t, countOutboxEvents(t, db, "ParticipantVerifiedV1", id),
			"the event INSERT must roll back with the business write")
	})

	t.Run("read model returns the profile", func(t *testing.T) {
		t.Parallel()
		repo := newRepo(t)
		readModels := adapters.NewPostgresReadModels(db)
		p := newRegistered(t)
		require.NoError(t, repo.Add(t.Context(), p))

		view, err := readModels.Profile(t.Context(), p.ID())
		require.NoError(t, err)
		assert.Equal(t, p.ID().String(), view.ParticipantID)
		assert.Equal(t, p.Email().String(), view.Email)
		assert.Equal(t, p.DisplayName(), view.DisplayName)
		assert.Equal(t, "registered", view.Status)
	})

	t.Run("read model maps missing row to NotFoundError", func(t *testing.T) {
		t.Parallel()
		readModels := adapters.NewPostgresReadModels(db)
		id, err := participant.NewParticipantID(uuid.NewString())
		require.NoError(t, err)

		_, err = readModels.Profile(t.Context(), id)

		var notFound participant.NotFoundError
		require.ErrorAs(t, err, &notFound)
	})
}

// failAfterVerify mutates the aggregate and then fails, sabotaging the
// transaction on purpose.
func failAfterVerify() func(ctx context.Context, p *participant.Participant) (*participant.Participant, error) {
	return func(_ context.Context, p *participant.Participant) (*participant.Participant, error) {
		if err := p.Verify(time.Now()); err != nil {
			return nil, err
		}
		return nil, assert.AnError
	}
}

// countOutboxEvents counts watermill outbox rows of the given event
// name carrying the participant id. The messages table name comes from
// the default schema adapter (`"watermill_<topic>"`, quoted identifier).
func countOutboxEvents(t *testing.T, db *sql.DB, eventName, participantID string) int {
	t.Helper()

	table := `"watermill_` + events.Topic + `"`
	var count int
	err := db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM `+table+`
		WHERE metadata->>'name' = $1 AND payload->>'participant_id' = $2`,
		eventName, participantID,
	).Scan(&count)
	require.NoError(t, err)
	return count
}
