package ports

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"molot/internal/auction/app/command"
	"molot/internal/auction/domain/auction"
	"molot/internal/common/decorator"
)

// closingCandidateLimit caps one tick's batch; lagging auctions are
// picked up by the next tick (§10).
const closingCandidateLimit = 100

// dueAuctionsScanner is the worker's consumer-side view of the
// candidate scan — a light, lock-free SELECT over the partial index.
type dueAuctionsScanner interface {
	DueForClosing(ctx context.Context, before time.Time, limit int) ([]auction.AuctionID, error)
}

type workerClock interface {
	Now() time.Time
}

// ClosingWorker closes auctions by time (§10): tick → scan candidates
// → CloseAuction command per id. Idempotency and races (anti-snipe
// extensions, concurrent replicas) are resolved by the aggregate
// guards plus the row lock — the worker never reasons about state.
type ClosingWorker struct {
	closeAuction decorator.CommandHandler[command.CloseAuction]
	scanner      dueAuctionsScanner
	clock        workerClock
	interval     time.Duration
	logger       *slog.Logger

	tracer       trace.Tracer
	tickDuration metric.Float64Histogram
	dueBacklog   metric.Int64Gauge
}

func NewClosingWorker(
	closeAuction decorator.CommandHandler[command.CloseAuction],
	scanner dueAuctionsScanner,
	clock workerClock,
	interval time.Duration,
	logger *slog.Logger,
	tracerProvider trace.TracerProvider,
	meterProvider metric.MeterProvider,
) *ClosingWorker {
	if closeAuction == nil {
		panic("NewClosingWorker: nil close handler")
	}
	if scanner == nil {
		panic("NewClosingWorker: nil scanner")
	}
	if clock == nil {
		panic("NewClosingWorker: nil clock")
	}
	if interval <= 0 {
		panic("NewClosingWorker: non-positive interval")
	}
	if logger == nil {
		panic("NewClosingWorker: nil logger")
	}
	if tracerProvider == nil {
		panic("NewClosingWorker: nil tracer provider")
	}
	if meterProvider == nil {
		panic("NewClosingWorker: nil meter provider")
	}

	meter := meterProvider.Meter("molot/internal/auction/ports")
	tickDuration, err := meter.Float64Histogram("molot_worker_tick_duration",
		metric.WithDescription("Duration of one worker tick"), metric.WithUnit("s"))
	if err != nil {
		panic("NewClosingWorker: create tick duration histogram: " + err.Error())
	}
	dueBacklog, err := meter.Int64Gauge("molot_worker_due_backlog",
		metric.WithDescription("Due candidates found by the last tick"))
	if err != nil {
		panic("NewClosingWorker: create due backlog gauge: " + err.Error())
	}

	return &ClosingWorker{
		closeAuction: closeAuction,
		scanner:      scanner,
		clock:        clock,
		interval:     interval,
		logger:       logger,
		tracer:       tracerProvider.Tracer("molot/internal/auction/ports"),
		tickDuration: tickDuration,
		dueBacklog:   dueBacklog,
	}
}

// Run ticks until ctx is cancelled; it always returns nil so a clean
// shutdown does not poison the errgroup.
func (w *ClosingWorker) Run(ctx context.Context) error {
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

func (w *ClosingWorker) tick(ctx context.Context) {
	ctx, span := w.tracer.Start(ctx, "worker/closing.tick")
	defer span.End()

	workerAttr := metric.WithAttributes(attribute.String("worker", "closing"))
	start := time.Now()
	defer func() {
		w.tickDuration.Record(ctx, time.Since(start).Seconds(), workerAttr)
	}()

	ids, err := w.scanner.DueForClosing(ctx, w.clock.Now(), closingCandidateLimit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.logger.ErrorContext(ctx, "closing worker scan failed", slog.Any("error", err))
		return
	}
	span.SetStatus(codes.Ok, "")
	w.dueBacklog.Record(ctx, int64(len(ids)), workerAttr)

	for _, id := range ids {
		if err := w.closeAuction.Handle(ctx, command.CloseAuction{AuctionID: id}); err != nil {
			// Benign outcomes (already closed / extended / cancelled)
			// are nil by the handler; anything here is infrastructure.
			w.logger.ErrorContext(ctx, "closing auction failed",
				slog.String("auction_id", id.String()), slog.Any("error", err))
		}
	}
}
