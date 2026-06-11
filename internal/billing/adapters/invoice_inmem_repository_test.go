package adapters_test

import (
	"testing"

	"molot/internal/billing/adapters"
	"molot/internal/billing/domain/invoice"
)

// TestInvoiceInMemoryRepository runs the shared suite against the
// in-memory implementation — no Docker required.
func TestInvoiceInMemoryRepository(t *testing.T) {
	t.Parallel()
	testInvoiceRepository(t, func(t *testing.T) invoice.Repository {
		t.Helper()
		return adapters.NewInvoiceInMemoryRepository()
	})
}
