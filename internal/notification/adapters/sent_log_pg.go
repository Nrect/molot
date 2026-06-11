package adapters

import (
	"context"
	"database/sql"
	"fmt"
)

// SentLogPG is the Postgres idempotency ledger over
// notification.sent_notifications (PK event_id, kind, recipient_id).
type SentLogPG struct {
	db *sql.DB
}

func NewSentLogPG(db *sql.DB) *SentLogPG {
	if db == nil {
		panic("notification.NewSentLogPG: nil db")
	}
	return &SentLogPG{db: db}
}

// FirstDelivery records the delivery slot and reports whether this call
// was the first to do so. The INSERT is a single-statement transaction
// that commits BEFORE the caller sends the email — deliberately not
// atomic with the send (sending is not transactional); a crash between
// commit and send loses that one email. The at-most-once-after-dedup
// trade-off is documented in the context README.
func (l *SentLogPG) FirstDelivery(ctx context.Context, eventID, kind, recipientID string) (bool, error) {
	res, err := l.db.ExecContext(ctx, `
		INSERT INTO notification.sent_notifications (event_id, kind, recipient_id, sent_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT DO NOTHING`,
		eventID, kind, recipientID)
	if err != nil {
		return false, fmt.Errorf("unable to record delivery %s/%s to %s: %w", eventID, kind, recipientID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unable to read rows affected for delivery %s/%s: %w", eventID, kind, err)
	}
	return inserted == 1, nil
}
