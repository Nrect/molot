// Package adapters holds the infrastructure side of the participant
// context: Postgres + in-memory repositories (one shared test suite),
// the read models, the domain→integration events mapper and the goose
// migrations.
package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/jackc/pgx/v5/pgconn"

	"molot/internal/common/postgres"
	"molot/internal/participant/domain/participant"
)

// pgParticipant is the transport struct of the participants row — the
// storage model is private to the adapter; the domain type carries no
// db tags (BOOK_AUDIT rule 7).
type pgParticipant struct {
	ID          string
	Email       string
	DisplayName string
	Status      string
	Version     int64
}

func (row pgParticipant) toDomain() (*participant.Participant, error) {
	p, err := participant.UnmarshalFromDatabase(row.ID, row.Email, row.DisplayName, row.Status, row.Version)
	if err != nil {
		return nil, fmt.Errorf("unable to map participant row %s: %w", row.ID, err)
	}
	return p, nil
}

// PostgresRepository persists Participant in participant.participants
// and publishes integration events through the transactional outbox.
type PostgresRepository struct {
	db     *sql.DB
	logger wm.LoggerAdapter
}

func NewPostgresRepository(db *sql.DB, logger wm.LoggerAdapter) *PostgresRepository {
	if db == nil {
		panic("NewPostgresRepository: nil db")
	}
	if logger == nil {
		panic("NewPostgresRepository: nil watermill logger")
	}
	return &PostgresRepository{db: db, logger: logger}
}

func (r *PostgresRepository) Add(ctx context.Context, p *participant.Participant) error {
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UTC()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO participant.participants
				(id, email, display_name, status, version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)`,
			p.ID().String(), p.Email().String(), p.DisplayName(), p.Status().String(), p.Version(), now,
		)
		if err != nil {
			if isUniqueViolation(err) {
				// Both unique constraints (pk = client-generated id,
				// email) mean a duplicate registration (repo contract).
				return fmt.Errorf("insert participant %s: %w", p.ID(), participant.ErrEmailTaken)
			}
			return fmt.Errorf("unable to insert participant: %w", err)
		}

		return publishIntegrationEvents(ctx, tx, r.logger, p.PullDomainEvents(), now)
	})
}

func (r *PostgresRepository) Get(ctx context.Context, id participant.ParticipantID) (*participant.Participant, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, email, display_name, status, version
		FROM participant.participants
		WHERE id = $1`, id.String(),
	)

	var transport pgParticipant
	err := row.Scan(&transport.ID, &transport.Email, &transport.DisplayName, &transport.Status, &transport.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, participant.NotFoundError{ID: id}
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get participant from db: %w", err)
	}

	return transport.toDomain()
}

func (r *PostgresRepository) Update(
	ctx context.Context,
	id participant.ParticipantID,
	actor participant.Actor,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	return r.update(ctx, id, func(p *participant.Participant) error {
		return participant.CanActorUpdateParticipant(actor, *p)
	}, updateFn)
}

func (r *PostgresRepository) UpdateAsOperations(
	ctx context.Context,
	id participant.ParticipantID,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	return r.update(ctx, id, nil, updateFn)
}

// update is the shared read-modify-write path: SELECT ... FOR UPDATE,
// authorization guard inside the transaction (rule 22), updateFn,
// persist with version increment (optimistic lock on top of the row
// lock), outbox publish — all in one transaction finished by the
// RunInTx idiom (named err + FinishTransaction).
func (r *PostgresRepository) update(
	ctx context.Context,
	id participant.ParticipantID,
	guard func(p *participant.Participant) error,
	updateFn func(ctx context.Context, p *participant.Participant) (*participant.Participant, error),
) error {
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT id, email, display_name, status, version
			FROM participant.participants
			WHERE id = $1
			FOR UPDATE`, id.String(),
		)

		var transport pgParticipant
		err := row.Scan(&transport.ID, &transport.Email, &transport.DisplayName, &transport.Status, &transport.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return participant.NotFoundError{ID: id}
		}
		if err != nil {
			return fmt.Errorf("unable to get participant for update: %w", err)
		}

		p, err := transport.toDomain()
		if err != nil {
			return err
		}

		if guard != nil {
			if err := guard(p); err != nil {
				return err
			}
		}

		updated, err := updateFn(ctx, p)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, `
			UPDATE participant.participants
			SET email = $2, display_name = $3, status = $4, version = $5, updated_at = $6
			WHERE id = $1 AND version = $7`,
			id.String(), updated.Email().String(), updated.DisplayName(), updated.Status().String(),
			transport.Version+1, now, transport.Version,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("update participant %s: %w", id, participant.ErrEmailTaken)
			}
			return fmt.Errorf("unable to update participant: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("unable to read update result: %w", err)
		}
		if affected == 0 {
			// Unreachable under FOR UPDATE; kept as belt-and-suspenders.
			return fmt.Errorf("concurrent modification of participant %s", id)
		}

		return publishIntegrationEvents(ctx, tx, r.logger, updated.PullDomainEvents(), now)
	})
}

// isUniqueViolation reports a Postgres unique_violation (23505) without
// letting driver errors leak above the repository (rule 19).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
