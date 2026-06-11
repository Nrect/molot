package adapters

import (
	"context"
	"fmt"
	"sync"
)

// RecipientsInMem is the in-memory recipient directory (BOOK_AUDIT
// rule 20): a map of values guarded by an RWMutex, behaviourally
// identical to RecipientsPG under the shared adapter suite.
type RecipientsInMem struct {
	mu   sync.RWMutex
	rows map[string]recipientRow
}

type recipientRow struct {
	email       string
	displayName string
}

func NewRecipientsInMem() *RecipientsInMem {
	return &RecipientsInMem{rows: make(map[string]recipientRow)}
}

func (r *RecipientsInMem) UpsertRecipient(_ context.Context, participantID, email, displayName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[participantID] = recipientRow{email: email, displayName: displayName}
	return nil
}

func (r *RecipientsInMem) RecipientByID(_ context.Context, participantID string) (string, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	row, ok := r.rows[participantID]
	if !ok {
		return "", "", fmt.Errorf("recipient %s is not in the directory yet", participantID)
	}
	return row.email, row.displayName, nil
}
