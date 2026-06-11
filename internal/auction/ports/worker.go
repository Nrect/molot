package ports

import (
	"context"
	"log/slog"
	"time"

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
}

func NewClosingWorker(
	closeAuction decorator.CommandHandler[command.CloseAuction],
	scanner dueAuctionsScanner,
	clock workerClock,
	interval time.Duration,
	logger *slog.Logger,
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
	return &ClosingWorker{
		closeAuction: closeAuction,
		scanner:      scanner,
		clock:        clock,
		interval:     interval,
		logger:       logger,
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
	ids, err := w.scanner.DueForClosing(ctx, w.clock.Now(), closingCandidateLimit)
	if err != nil {
		w.logger.ErrorContext(ctx, "closing worker scan failed", slog.Any("error", err))
		return
	}
	for _, id := range ids {
		if err := w.closeAuction.Handle(ctx, command.CloseAuction{AuctionID: id}); err != nil {
			// Benign outcomes (already closed / extended / cancelled)
			// are nil by the handler; anything here is infrastructure.
			w.logger.ErrorContext(ctx, "closing auction failed",
				slog.String("auction_id", id.String()), slog.Any("error", err))
		}
	}
}
