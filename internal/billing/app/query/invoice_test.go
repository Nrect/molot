package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/app/query"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/errs"
)

// readModelSpy plays one scripted view or a not-found.
type readModelSpy struct {
	view    query.InvoiceView
	missing bool
}

func (s readModelSpy) InvoiceByID(_ context.Context, id invoice.InvoiceID, _ invoice.BidderID) (query.InvoiceView, error) {
	if s.missing {
		return query.InvoiceView{}, invoice.NotFoundError{InvoiceID: id}
	}
	return s.view, nil
}

func (s readModelSpy) PendingOfBidder(context.Context, invoice.BidderID) ([]query.InvoiceView, error) {
	return []query.InvoiceView{s.view}, nil
}

func TestInvoiceByIDHandler(t *testing.T) {
	t.Parallel()

	id, err := invoice.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	actor, err := invoice.NewBidderID(uuid.New())
	require.NoError(t, err)

	t.Run("returns the view", func(t *testing.T) {
		t.Parallel()
		want := query.InvoiceView{InvoiceID: id.String(), Status: "pending", TotalMinor: 110_000, DueAt: time.Now()}
		h := query.NewInvoiceByIDHandler(readModelSpy{view: want})

		got, err := h.Handle(context.Background(), query.InvoiceByID{InvoiceID: id, Actor: actor})
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("missing or foreign invoice maps to the 404 slug", func(t *testing.T) {
		t.Parallel()
		h := query.NewInvoiceByIDHandler(readModelSpy{missing: true})

		_, err := h.Handle(context.Background(), query.InvoiceByID{InvoiceID: id, Actor: actor})
		assert.True(t, errors.Is(err, errs.NewNotFoundError("invoice-not-found")))
	})
}
