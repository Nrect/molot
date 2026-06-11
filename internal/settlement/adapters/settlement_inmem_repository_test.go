package adapters_test

import (
	"testing"

	"molot/internal/settlement/adapters"
	"molot/internal/settlement/domain/settlement"
)

// TestSettlementInMemoryRepository runs the shared suite against the
// in-memory adapter — the no-Docker leg of rule 42.
func TestSettlementInMemoryRepository(t *testing.T) {
	t.Parallel()
	testSettlementRepository(t, func(t *testing.T) settlement.Repository {
		t.Helper()
		return adapters.NewSettlementInMemoryRepository()
	})
}
