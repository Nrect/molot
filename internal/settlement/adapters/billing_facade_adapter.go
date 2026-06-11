package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	billingservice "molot/internal/billing/service"
	"molot/internal/common/errs"
	"molot/internal/settlement/domain/settlement"
)

// BillingFacade is the slice of the billing service facade the
// settlement saga consumes (§6, ADR-0004); satisfied by
// billingservice.Facade — see AuctionFacade for the import rationale.
type BillingFacade interface {
	IssueInvoice(ctx context.Context, invoiceID, auctionID, debtor uuid.UUID,
		hammerMinor int64, currency string, attempt int) error
	VoidInvoice(ctx context.Context, invoiceID uuid.UUID) error
}

var _ BillingFacade = billingservice.Facade{}

// BillingFacadeAdapter implements the saga's consumer-side billing
// gateways over the synchronous facade with facade/ spans (§11).
type BillingFacadeAdapter struct {
	facade BillingFacade
	tracer trace.Tracer
}

func NewBillingFacadeAdapter(facade BillingFacade, tracerProvider trace.TracerProvider) BillingFacadeAdapter {
	if facade == nil {
		panic("NewBillingFacadeAdapter: nil facade")
	}
	if tracerProvider == nil {
		panic("NewBillingFacadeAdapter: nil tracer provider")
	}
	return BillingFacadeAdapter{
		facade: facade,
		tracer: tracerProvider.Tracer("molot/internal/settlement/adapters"),
	}
}

// IssueInvoice issues the attempt's bill. Idempotent on the billing
// side: the deterministic invoice id and UNIQUE(auction_id, attempt)
// turn a repeat into a no-op (nil).
func (a BillingFacadeAdapter) IssueInvoice(
	ctx context.Context,
	invoiceID settlement.InvoiceID,
	auctionID settlement.AuctionID,
	debtor settlement.BidderID,
	amount settlement.Money,
	attempt int,
) error {
	return a.traced(ctx, "facade/billing.IssueInvoice", func(ctx context.Context) error {
		return a.facade.IssueInvoice(ctx,
			invoiceID.UUID(), auctionID.UUID(), debtor.UUID(),
			amount.Amount(), amount.Currency().String(), attempt,
		)
	})
}

// VoidInvoice cancels the pending second-chance invoice. The adapter
// translates billing's "invoice-already-paid" conflict into the saga's
// own language — settlement.ErrOfferAlreadyPaid — so the decline
// handler never matches a foreign slug (§6.4). Already voided/expired
// is nil on the billing side (idempotent no-op).
func (a BillingFacadeAdapter) VoidInvoice(ctx context.Context, invoiceID settlement.InvoiceID) error {
	return a.traced(ctx, "facade/billing.VoidInvoice", func(ctx context.Context) error {
		err := a.facade.VoidInvoice(ctx, invoiceID.UUID())
		if errors.Is(err, errs.NewConflictError("invoice-already-paid")) {
			return fmt.Errorf("%w: %w", settlement.ErrOfferAlreadyPaid, err)
		}
		return err
	})
}

func (a BillingFacadeAdapter) traced(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx, span := a.tracer.Start(ctx, name)
	defer span.End()
	err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
