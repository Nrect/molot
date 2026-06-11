package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"molot/internal/participant/app/query"
	"molot/internal/participant/domain/participant"
)

// PostgresReadModels serves the query side of the context by reading
// the write table directly (ARCHITECTURE.md §5: "not every query needs
// a read model").
type PostgresReadModels struct {
	db *sql.DB
}

func NewPostgresReadModels(db *sql.DB) *PostgresReadModels {
	if db == nil {
		panic("NewPostgresReadModels: nil db")
	}
	return &PostgresReadModels{db: db}
}

func (rm *PostgresReadModels) Profile(ctx context.Context, id participant.ParticipantID) (query.ProfileView, error) {
	row := rm.db.QueryRowContext(ctx, `
		SELECT id, email, display_name, status
		FROM participant.participants
		WHERE id = $1`, id.String(),
	)

	var view query.ProfileView
	err := row.Scan(&view.ParticipantID, &view.Email, &view.DisplayName, &view.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return query.ProfileView{}, participant.NotFoundError{ID: id}
	}
	if err != nil {
		return query.ProfileView{}, fmt.Errorf("unable to read participant profile: %w", err)
	}
	return view, nil
}
