package adapters_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/billing/adapters"
	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
)

func chargeKey(t *testing.T) command.ChargeKey {
	t.Helper()
	id, err := invoice.NewInvoiceID(uuid.New())
	require.NoError(t, err)
	return command.ChargeKey(id)
}

func amount(t *testing.T) invoice.Money {
	t.Helper()
	cur, err := invoice.NewCurrency("USD")
	require.NoError(t, err)
	m, err := invoice.NewMoney(110_000, cur)
	require.NoError(t, err)
	return m
}

func TestNewFakePSPRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	_, err := adapters.NewFakePSP("sometimes")
	assert.Error(t, err)
}

func TestFakePSPSuccessChargeIsIdempotentByKey(t *testing.T) {
	t.Parallel()
	psp, err := adapters.NewFakePSP(adapters.PSPModeSuccess)
	require.NoError(t, err)
	key := chargeKey(t)

	first, err := psp.Charge(context.Background(), key, amount(t))
	require.NoError(t, err)
	assert.False(t, first.IsZero())

	second, err := psp.Charge(context.Background(), key, amount(t))
	require.NoError(t, err)
	assert.Equal(t, first, second, "same idempotency key must return the same reference, no double debit")

	other, err := psp.Charge(context.Background(), chargeKey(t), amount(t))
	require.NoError(t, err)
	assert.NotEqual(t, first, other, "different keys are independent charges")
}

func TestFakePSPDeclineRefusesEveryCharge(t *testing.T) {
	t.Parallel()
	psp, err := adapters.NewFakePSP(adapters.PSPModeDecline)
	require.NoError(t, err)
	key := chargeKey(t)

	for range 2 {
		_, chargeErr := psp.Charge(context.Background(), key, amount(t))
		assert.ErrorIs(t, chargeErr, invoice.ErrPaymentDeclined)
	}
	assert.False(t, psp.HasCharge(key), "a declined charge must not record a debit")
}

func TestFakePSPFlakyFailsFirstThenSucceeds(t *testing.T) {
	t.Parallel()
	psp, err := adapters.NewFakePSP(adapters.PSPModeFlaky)
	require.NoError(t, err)
	key := chargeKey(t)

	_, chargeErr := psp.Charge(context.Background(), key, amount(t))
	assert.ErrorIs(t, chargeErr, invoice.ErrPSPUnavailable, "first charge per key is a network error")

	ref, err := psp.Charge(context.Background(), key, amount(t))
	require.NoError(t, err, "the retry succeeds")
	assert.False(t, ref.IsZero())

	again, err := psp.Charge(context.Background(), key, amount(t))
	require.NoError(t, err)
	assert.Equal(t, ref, again, "after success the charge is idempotent")

	// Per-key flakiness: a fresh key fails its own first attempt.
	_, chargeErr = psp.Charge(context.Background(), chargeKey(t), amount(t))
	assert.ErrorIs(t, chargeErr, invoice.ErrPSPUnavailable)
}

func TestFakePSPRefund(t *testing.T) {
	t.Parallel()
	psp, err := adapters.NewFakePSP(adapters.PSPModeSuccess)
	require.NoError(t, err)

	t.Run("no-op without a charge", func(t *testing.T) {
		t.Parallel()
		key := chargeKey(t)
		assert.NoError(t, psp.Refund(context.Background(), key))
		assert.False(t, psp.IsRefunded(key))
	})

	t.Run("refunds an existing charge idempotently", func(t *testing.T) {
		t.Parallel()
		key := chargeKey(t)
		_, err := psp.Charge(context.Background(), key, amount(t))
		require.NoError(t, err)

		require.NoError(t, psp.Refund(context.Background(), key))
		assert.True(t, psp.IsRefunded(key))
		require.NoError(t, psp.Refund(context.Background(), key), "repeating a refund is a no-op")
		assert.True(t, psp.IsRefunded(key))
	})
}
