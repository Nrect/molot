package query_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/errs"
	"molot/internal/settlement/app/query"
	"molot/internal/settlement/domain/settlement"
)

// readModelSpy is a recording spy (rule 41) playing a scripted view.
type readModelSpy struct {
	view    query.SettlementView
	err     error
	queried []settlement.AuctionID
}

func (s *readModelSpy) SettlementByAuction(_ context.Context, id settlement.AuctionID) (query.SettlementView, error) {
	s.queried = append(s.queried, id)
	if s.err != nil {
		return query.SettlementView{}, s.err
	}
	return s.view, nil
}

func TestSettlementStatus(t *testing.T) {
	t.Parallel()

	auctionID, err := settlement.NewAuctionID(uuid.New())
	require.NoError(t, err)

	t.Run("returns the view for the queried auction", func(t *testing.T) {
		t.Parallel()
		spy := &readModelSpy{view: query.SettlementView{AuctionID: auctionID.String(), State: "settled"}}
		h := query.NewSettlementStatusHandler(spy)

		view, err := h.Handle(context.Background(), query.SettlementStatus{AuctionID: auctionID})
		require.NoError(t, err)
		assert.Equal(t, "settled", view.State)
		assert.Equal(t, []settlement.AuctionID{auctionID}, spy.queried)
	})

	t.Run("maps a missing settlement to the 404 slug", func(t *testing.T) {
		t.Parallel()
		spy := &readModelSpy{err: settlement.NotFoundError{AuctionID: auctionID}}
		h := query.NewSettlementStatusHandler(spy)

		_, err := h.Handle(context.Background(), query.SettlementStatus{AuctionID: auctionID})
		assert.ErrorIs(t, err, errs.NewNotFoundError("settlement-not-found"))
	})
}
