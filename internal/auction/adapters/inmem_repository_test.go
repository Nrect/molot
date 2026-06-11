package adapters_test

import (
	"testing"

	"molot/internal/auction/adapters"
)

// The in-memory half of the shared suite runs at the unit level —
// no Docker required.
func TestAuctionInMemRepository(t *testing.T) {
	t.Parallel()
	runRepositorySuite(t, func(t *testing.T) testRepository {
		return adapters.NewAuctionInMemRepository()
	})
}
