package ports

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/decorator"
)

// expiryBatchLimit caps one tick's candidate scan (§10).
const expiryBatchLimit = 100

// dueInvoices is the worker's consumer-side slice of the repository:
// the lock-free candidate scan backed by the partial index
// invoices_due_idx. Idempotency lives in the aggregate's guard table,
// not here — a repeated or concurrent expiry is a no-op.
type dueInvoices interface {
	PendingDueBefore(ctx context.Context, t time.Time, limit int) ([]invoice.InvoiceID, error)
}

type workerClock interface {
	Now() time.Time
}

// ExpiryWorker is the payment-timeout worker (ARCHITECTURE.md §10): a
// ticker scans pending invoices past due_at and drives each through the
// ordinary ExpireInvoice command (UpdateAsSystem → guard table). The
// deadline itself is invoice data (due_at = issued_at + PAYMENT_TERM);
// the saga never sleeps — it only reacts to InvoiceExpiredV1.
type ExpiryWorker struct {
	due      dueInvoices
	expire   decorator.CommandHandler[command.ExpireInvoice]
	interval time.Duration
	clock    workerClock
	logger   *slog.Logger

	tracer       trace.Tracer
	tickDuration metric.Float64Histogram
	dueBacklog   metric.Int64Gauge
}

func NewExpiryWorker(
	due dueInvoices,
	expire decorator.CommandHandler[command.ExpireInvoice],
	interval time.Duration,
	clk workerClock,
	logger *slog.Logger,
	meterProvider metric.MeterProvider,
	tracerProvider trace.TracerProvider,
) *ExpiryWorker {
	if due == nil {
		panic("NewExpiryWorker: nil due scan")
	}
	if expire == nil {
		panic("NewExpiryWorker: nil expire handler")
	}
	if interval <= 0 {
		panic("NewExpiryWorker: non-positive interval")
	}
	if clk == nil {
		panic("NewExpiryWorker: nil clock")
	}
	if logger == nil {
		panic("NewExpiryWorker: nil logger")
	}
	if meterProvider == nil {
		panic("NewExpiryWorker: nil meter provider")
	}
	if tracerProvider == nil {
		panic("NewExpiryWorker: nil tracer provider")
	}

	meter := meterProvider.Meter("molot/internal/billing/ports")
	tickDuration, err := meter.Float64Histogram("molot_worker_tick_duration",
		metric.WithDescription("Duration of one worker tick"), metric.WithUnit("s"))
	if err != nil {
		panic("NewExpiryWorker: create tick duration histogram: " + err.Error())
	}
	dueBacklog, err := meter.Int64Gauge("molot_worker_due_backlog",
		metric.WithDescription("Due candidates found by the last tick"))
	if err != nil {
		panic("NewExpiryWorker: create due backlog gauge: " + err.Error())
	}

	return &ExpiryWorker{
		due:          due,
		expire:       expire,
		interval:     interval,
		clock:        clk,
		logger:       logger,
		tracer:       tracerProvider.Tracer("molot/internal/billing/ports"),
		tickDuration: tickDuration,
		dueBacklog:   dueBacklog,
	}
}

// Run ticks until ctx is cancelled; it always returns nil on shutdown
// so the errgroup of main treats cancellation as a clean stop.
func (w *ExpiryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *ExpiryWorker) tick(ctx context.Context) {
	ctx, span := w.tracer.Start(ctx, "worker/expiry.tick")
	defer span.End()

	workerAttr := metric.WithAttributes(attribute.String("worker", "expiry"))
	start := time.Now()
	defer func() {
		w.tickDuration.Record(ctx, time.Since(start).Seconds(), workerAttr)
	}()

	ids, err := w.due.PendingDueBefore(ctx, w.clock.Now(), expiryBatchLimit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.logger.ErrorContext(ctx, "expiry worker: due scan failed",
			slog.String("context", "billing"), slog.Any("error", err))
		return
	}
	span.SetStatus(codes.Ok, "")
	w.dueBacklog.Record(ctx, int64(len(ids)), workerAttr)

	for _, id := range ids {
		// Errors are logged and the batch continues: the next tick
		// retries naturally, and already-terminal invoices are no-ops
		// by the guard table.
		if err := w.expire.Handle(ctx, command.ExpireInvoice{InvoiceID: id}); err != nil {
			w.logger.ErrorContext(ctx, "expiry worker: expire failed",
				slog.String("context", "billing"),
				slog.String("invoice_id", id.String()),
				slog.Any("error", err))
		}
	}
}
