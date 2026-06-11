package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RecipientsPG is the Postgres recipient directory: the mini-projection
// participant_id → (email, display_name) fed by participant-events.
type RecipientsPG struct {
	db *sql.DB
}

func NewRecipientsPG(db *sql.DB) *RecipientsPG {
	if db == nil {
		panic("notification.NewRecipientsPG: nil db")
	}
	return &RecipientsPG{db: db}
}

// UpsertRecipient is idempotent: a redelivered registration overwrites
// the row with the same values.
func (r *RecipientsPG) UpsertRecipient(ctx context.Context, participantID, email, displayName string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notification.recipients (participant_id, email, display_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (participant_id)
		DO UPDATE SET email = excluded.email, display_name = excluded.display_name`,
		participantID, email, displayName)
	if err != nil {
		return fmt.Errorf("unable to upsert recipient %s: %w", participantID, err)
	}
	return nil
}

// RecipientByID resolves a participant to their address. An unknown
// participant is projection lag (the registration event is not consumed
// yet): the returned error makes the bus redeliver the notification
// until the directory catches up. The driver's not-found does not leak
// above the adapter (BOOK_AUDIT rule 19).
func (r *RecipientsPG) RecipientByID(ctx context.Context, participantID string) (string, string, error) {
	var email, displayName string
	err := r.db.QueryRowContext(ctx,
		`SELECT email, display_name FROM notification.recipients WHERE participant_id = $1`,
		participantID).Scan(&email, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("recipient %s is not in the directory yet", participantID)
	}
	if err != nil {
		return "", "", fmt.Errorf("unable to query recipient %s: %w", participantID, err)
	}
	return email, displayName, nil
}
