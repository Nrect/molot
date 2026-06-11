// Package query is the read side of the settlement context: the
// operations-only saga status view, read directly from the write table
// (§5 — "not every query needs a read model"); the interface is
// declared next to its consumer.
package query

import (
	"context"
	"errors"

	"molot/internal/common/errs"
	"molot/internal/settlement/domain/settlement"
)

// SettlementView is the UI-shaped read result — not the domain
// aggregate, not an OpenAPI model, not a DB row (rule 26).
type SettlementView struct {
	AuctionID         string
	State             string
	WinnerID          string
	HammerMinor       int64
	Currency          string
	RunnerUpID        string // "" when there was no runner-up
	RunnerUpMinor     int64
	RunnerUpQualifies bool
	RelistGeneration  int
	Attempt           int
	InvoiceID         string // "" until known
	FailureReason     string // "" until Relisted / FailedUnsold
}

// SettlementReadModel reads the saga state for operations.
type SettlementReadModel interface {
	SettlementByAuction(ctx context.Context, id settlement.AuctionID) (SettlementView, error)
}

// SettlementStatus returns the saga state of one auction (operations
// role; the role check lives in the HTTP port).
type SettlementStatus struct {
	AuctionID settlement.AuctionID
}

type SettlementStatusHandler struct {
	readModel SettlementReadModel
}

func NewSettlementStatusHandler(readModel SettlementReadModel) SettlementStatusHandler {
	if readModel == nil {
		panic("NewSettlementStatusHandler: nil read model")
	}
	return SettlementStatusHandler{readModel: readModel}
}

func (h SettlementStatusHandler) Handle(ctx context.Context, q SettlementStatus) (SettlementView, error) {
	view, err := h.readModel.SettlementByAuction(ctx, q.AuctionID)
	if err != nil {
		var notFound settlement.NotFoundError
		if errors.As(err, &notFound) {
			return SettlementView{}, errs.NewNotFoundError("settlement-not-found").WithCause(err)
		}
		return SettlementView{}, err
	}
	return view, nil
}
