package adapters

import (
	"context"
	"sync"
)

// SentLogInMem is the in-memory idempotency ledger (BOOK_AUDIT rule
// 20), behaviourally identical to SentLogPG under the shared adapter
// suite: exactly one FirstDelivery per (event, kind, recipient) wins,
// including under concurrency.
type SentLogInMem struct {
	mu   sync.Mutex
	seen map[deliveryKey]struct{}
}

type deliveryKey struct {
	eventID     string
	kind        string
	recipientID string
}

func NewSentLogInMem() *SentLogInMem {
	return &SentLogInMem{seen: make(map[deliveryKey]struct{})}
}

func (l *SentLogInMem) FirstDelivery(_ context.Context, eventID, kind, recipientID string) (bool, error) {
	key := deliveryKey{eventID: eventID, kind: kind, recipientID: recipientID}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, duplicate := l.seen[key]; duplicate {
		return false, nil
	}
	l.seen[key] = struct{}{}
	return true, nil
}
