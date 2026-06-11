package ports_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
	"molot/internal/billing/ports"
)

// dueListerStub returns one scripted batch on the first scan, then nothing.
type dueListerStub struct {
	mu    sync.Mutex
	batch []invoice.InvoiceID
}

func (s *dueListerStub) PendingDueBefore(context.Context, time.Time, int) ([]invoice.InvoiceID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.batch
	s.batch = nil
	return batch, nil
}

// expireSpy records every handled command and signals on a channel.
type expireSpy struct {
	mu      sync.Mutex
	handled []invoice.InvoiceID
	errFor  map[invoice.InvoiceID]error
	done    chan invoice.InvoiceID
}

func (s *expireSpy) Handle(_ context.Context, cmd command.ExpireInvoice) error {
	s.mu.Lock()
	s.handled = append(s.handled, cmd.InvoiceID)
	err := s.errFor[cmd.InvoiceID]
	s.mu.Unlock()
	s.done <- cmd.InvoiceID
	return err
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Now() }

func newInvoiceID(t *testing.T) invoice.InvoiceID {
	t.Helper()
	id, err := invoice.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	return id
}

// TestExpiryWorkerExpiresDueInvoices: one tick drives every scanned id
// through the ExpireInvoice command; a failing id does not stop the
// batch (the next tick retries naturally).
func TestExpiryWorkerExpiresDueInvoices(t *testing.T) {
	t.Parallel()

	failing, healthy := newInvoiceID(t), newInvoiceID(t)
	due := &dueListerStub{batch: []invoice.InvoiceID{failing, healthy}}
	spy := &expireSpy{
		errFor: map[invoice.InvoiceID]error{failing: errors.New("transient db error")},
		done:   make(chan invoice.InvoiceID, 2),
	}
	worker := ports.NewExpiryWorker(
		due, spy, time.Millisecond, stubClock{},
		slog.New(slog.DiscardHandler), metricnoop.NewMeterProvider(), tracenoop.NewTracerProvider(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- worker.Run(ctx) }()

	for range 2 {
		select {
		case <-spy.done:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not process the due batch in time")
		}
	}
	cancel()
	require.NoError(t, <-stopped, "cancellation is a clean stop")

	spy.mu.Lock()
	defer spy.mu.Unlock()
	assert.ElementsMatch(t, []invoice.InvoiceID{failing, healthy}, spy.handled[:2],
		"the batch continues past a failing invoice")
}
