package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"molot/internal/billing/adapters"
	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
)

// recordingGateway is a minimal stub that returns whatever its fields say.
type recordingGateway struct {
	chargeRef invoice.PaymentReference
	chargeErr error
	refundErr error

	chargeCallCount int
	refundCallCount int
}

func (r *recordingGateway) Charge(_ context.Context, _ command.ChargeKey, _ invoice.Money) (invoice.PaymentReference, error) {
	r.chargeCallCount++
	return r.chargeRef, r.chargeErr
}

func (r *recordingGateway) Refund(_ context.Context, _ command.ChargeKey) error {
	r.refundCallCount++
	return r.refundErr
}

// newTestInstrumentedPSP builds an InstrumentedPSP with a no-op tracer and a
// manual-reader meter so calls do not require an actual OTLP endpoint.
func newTestInstrumentedPSP(t *testing.T, gw *recordingGateway) *adapters.InstrumentedPSP {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tp := noop.NewTracerProvider()

	ins, err := adapters.NewInstrumentedPSP(gw, mp, tp)
	require.NoError(t, err)
	return ins
}

func TestInstrumentedPSPChargeForwardsSuccess(t *testing.T) {
	t.Parallel()

	ref, err := invoice.NewPaymentReference("psp-test-ref")
	require.NoError(t, err)

	gw := &recordingGateway{chargeRef: ref}
	ins := newTestInstrumentedPSP(t, gw)

	got, err := ins.Charge(context.Background(), chargeKey(t), amount(t))
	require.NoError(t, err)
	assert.Equal(t, ref, got)
	assert.Equal(t, 1, gw.chargeCallCount)
}

func TestInstrumentedPSPChargeForwardsDeclined(t *testing.T) {
	t.Parallel()

	gw := &recordingGateway{
		chargeErr: errors.New("bank says no: " + invoice.ErrPaymentDeclined.Error()),
	}
	// Wrap in ErrPaymentDeclined so errors.Is matches.
	gw.chargeErr = invoice.ErrPaymentDeclined

	ins := newTestInstrumentedPSP(t, gw)

	_, err := ins.Charge(context.Background(), chargeKey(t), amount(t))
	assert.ErrorIs(t, err, invoice.ErrPaymentDeclined)
	assert.Equal(t, 1, gw.chargeCallCount)
}

func TestInstrumentedPSPChargeForwardsUnavailable(t *testing.T) {
	t.Parallel()

	gw := &recordingGateway{chargeErr: invoice.ErrPSPUnavailable}
	ins := newTestInstrumentedPSP(t, gw)

	_, err := ins.Charge(context.Background(), chargeKey(t), amount(t))
	assert.ErrorIs(t, err, invoice.ErrPSPUnavailable)
}

func TestInstrumentedPSPRefundForwardsSuccess(t *testing.T) {
	t.Parallel()

	gw := &recordingGateway{}
	ins := newTestInstrumentedPSP(t, gw)

	err := ins.Refund(context.Background(), chargeKey(t))
	require.NoError(t, err)
	assert.Equal(t, 1, gw.refundCallCount)
}

func TestInstrumentedPSPRefundForwardsError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("refund network failure")
	gw := &recordingGateway{refundErr: sentinel}
	ins := newTestInstrumentedPSP(t, gw)

	err := ins.Refund(context.Background(), chargeKey(t))
	assert.ErrorIs(t, err, sentinel)
}

func TestNewInstrumentedPSPPanicsOnNilGateway(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	assert.Panics(t, func() {
		_, _ = adapters.NewInstrumentedPSP(nil, mp, noop.NewTracerProvider())
	})
}
