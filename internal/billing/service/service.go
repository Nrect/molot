// Package service is the composition root of the billing context: it
// assembles the Application (handlers wrapped with the common
// decorators), the HTTP port, the expiry worker and the synchronous
// Facade for the settlement saga. Billing subscribes to no integration
// events — it only publishes (§4.3), so there is no
// RegisterEventHandlers here by design.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"molot/internal/billing/adapters"
	"molot/internal/billing/app"
	"molot/internal/billing/app/command"
	"molot/internal/billing/app/query"
	"molot/internal/billing/domain/invoice"
	"molot/internal/billing/ports"
	"molot/internal/common/decorator"
)

// Deps carries everything NewService needs from the monolith's
// composition root. Pointers/interfaces must be non-nil (the
// constructor panics, BOOK_AUDIT rule 6); value fields are validated
// and reported as errors.
type Deps struct {
	Logger          *slog.Logger
	DB              *sql.DB
	WatermillLogger wm.LoggerAdapter
	MeterProvider   metric.MeterProvider
	TracerProvider  trace.TracerProvider

	// CommissionBasisPoints is the platform commission rate (bp);
	// CommissionPolicy is built here and snapshotted onto every
	// invoice at issue time.
	CommissionBasisPoints int
	// PaymentTerm: due_at = issued_at + PaymentTerm (env PAYMENT_TERM).
	PaymentTerm time.Duration
	// PSPMode steers the fake PSP: success | decline | flaky (env PSP_MODE).
	PSPMode string
	// ExpiryPollInterval is the expiry worker tick (env EXPIRY_POLL_INTERVAL).
	ExpiryPollInterval time.Duration
}

// Service is the billing context plugged into the monolith.
type Service struct {
	app    app.Application
	worker *ports.ExpiryWorker
	facade Facade
}

func NewService(deps Deps) (*Service, error) {
	if deps.Logger == nil {
		panic("billing.NewService: nil logger")
	}
	if deps.DB == nil {
		panic("billing.NewService: nil db")
	}
	if deps.WatermillLogger == nil {
		panic("billing.NewService: nil watermill logger")
	}
	if deps.MeterProvider == nil {
		panic("billing.NewService: nil meter provider")
	}
	if deps.TracerProvider == nil {
		panic("billing.NewService: nil tracer provider")
	}

	policy, err := invoice.NewCommissionPolicy(deps.CommissionBasisPoints)
	if err != nil {
		return nil, fmt.Errorf("billing: commission policy: %w", err)
	}
	term, err := invoice.NewPaymentTerm(deps.PaymentTerm)
	if err != nil {
		return nil, fmt.Errorf("billing: payment term: %w", err)
	}
	psp, err := adapters.NewFakePSP(deps.PSPMode)
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	if deps.ExpiryPollInterval <= 0 {
		return nil, fmt.Errorf("billing: expiry poll interval must be positive, got %s", deps.ExpiryPollInterval)
	}

	decorators, err := decorator.NewDecorators("billing", deps.Logger, deps.MeterProvider, deps.TracerProvider)
	if err != nil {
		return nil, fmt.Errorf("billing: decorators: %w", err)
	}

	repo := adapters.NewInvoicePostgresRepository(deps.DB, deps.Logger, deps.WatermillLogger)
	clk := systemClock{}

	application := app.Application{
		Commands: app.Commands{
			IssueInvoice: decorator.ApplyCommandDecorators[command.IssueInvoice](
				command.NewIssueInvoiceHandler(repo, policy, term, clk), decorators),
			PayInvoice: decorator.ApplyCommandDecorators[command.PayInvoice](
				command.NewPayInvoiceHandler(repo, psp, clk), decorators),
			ExpireInvoice: decorator.ApplyCommandDecorators[command.ExpireInvoice](
				command.NewExpireInvoiceHandler(repo, clk), decorators),
			VoidInvoice: decorator.ApplyCommandDecorators[command.VoidInvoice](
				command.NewVoidInvoiceHandler(repo, clk), decorators),
		},
		Queries: app.Queries{
			InvoiceByID: decorator.ApplyQueryDecorators[query.InvoiceByID, query.InvoiceView](
				query.NewInvoiceByIDHandler(repo), decorators),
			PendingInvoicesOfBidder: decorator.ApplyQueryDecorators[query.PendingInvoicesOfBidder, []query.InvoiceView](
				query.NewPendingInvoicesOfBidderHandler(repo), decorators),
		},
	}

	worker := ports.NewExpiryWorker(
		repo, application.Commands.ExpireInvoice,
		deps.ExpiryPollInterval, clk, deps.Logger, deps.MeterProvider,
	)

	return &Service{
		app:    application,
		worker: worker,
		facade: newFacade(application),
	}, nil
}

// RegisterHTTP mounts the billing endpoints on the authenticated /api group.
func (s *Service) RegisterHTTP(api chi.Router) {
	ports.RegisterHTTP(api, s.app)
}

// ExpiryWorker returns the worker run-function for the main errgroup:
// g.Go(billingSvc.ExpiryWorker(ctx)).
func (s *Service) ExpiryWorker(ctx context.Context) func() error {
	return func() error { return s.worker.Run(ctx) }
}

// Facade is the synchronous entry point for the settlement saga.
func (s *Service) Facade() Facade {
	return s.facade
}

// Migrations exposes the context's embedded goose migrations for the
// monolith's migration runner (per-context version table
// goose_db_version_billing).
func Migrations() fs.FS {
	return adapters.Migrations()
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
