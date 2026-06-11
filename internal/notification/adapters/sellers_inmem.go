package adapters

import (
	"context"
	"fmt"
	"sync"
)

// SellersInMem is the in-memory seller directory (BOOK_AUDIT rule 20),
// behaviourally identical to SellersPG under the shared adapter suite.
type SellersInMem struct {
	mu   sync.RWMutex
	rows map[string]string // auction id → seller id
}

func NewSellersInMem() *SellersInMem {
	return &SellersInMem{rows: make(map[string]string)}
}

func (s *SellersInMem) RememberSeller(_ context.Context, auctionID, sellerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[auctionID] = sellerID
	return nil
}

func (s *SellersInMem) SellerOf(_ context.Context, auctionID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sellerID, ok := s.rows[auctionID]
	if !ok {
		return "", fmt.Errorf("seller of auction %s is not known yet", auctionID)
	}
	return sellerID, nil
}
