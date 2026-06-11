package ports_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"molot/internal/auction/app/command"
	"molot/internal/auction/domain/auction"
	"molot/internal/auction/ports"
)

type stubClock struct{ now time.Time }

func (c stubClock) Now() time.Time { return c.now }

type stubScanner struct {
	mu  sync.Mutex
	ids []auction.AuctionID
}

func (s *stubScanner) DueForClosing(context.Context, time.Time, int) ([]auction.AuctionID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.ids
	s.ids = nil // hand the batch out once
	return ids, nil
}

type recordingCloseHandler struct {
	got chan command.CloseAuction
}

func (h recordingCloseHandler) Handle(_ context.Context, cmd command.CloseAuction) error {
	h.got <- cmd
	return nil
}

// The worker is orchestration only: scan → CloseAuction per candidate.
func TestClosingWorker_ClosesScannedCandidates(t *testing.T) {
	t.Parallel()

	id, err := auction.NewAuctionID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	handler := recordingCloseHandler{got: make(chan command.CloseAuction, 1)}
	scanner := &stubScanner{ids: []auction.AuctionID{id}}
	clock := stubClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}

	worker := ports.NewClosingWorker(handler, scanner, clock, time.Millisecond, slog.Default(),
		tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case cmd := <-handler.got:
		if cmd.AuctionID != id {
			t.Fatalf("closed %v, want %v", cmd.AuctionID, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker never issued CloseAuction")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker returned %v on clean shutdown, want nil", err)
	}
}
