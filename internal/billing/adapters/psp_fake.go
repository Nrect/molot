package adapters

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
)

// PSP modes, mirroring config.PSPMode values (the adapter deliberately
// takes a plain string so it is usable with any config source).
const (
	PSPModeSuccess = "success"
	PSPModeDecline = "decline"
	PSPModeFlaky   = "flaky"
)

// FakePSP is the fake payment provider honoring the PSP contract of
// §3.1: Charge is idempotent per key (a retry returns the original
// reference, never a second debit), Refund is idempotent and a no-op
// without a charge. The outcome is steered by PSP_MODE:
//
//   - success: every charge succeeds;
//   - decline: every charge is refused (invoice.ErrPaymentDeclined);
//   - flaky:   the FIRST Charge per key fails with a network error
//     (invoice.ErrPSPUnavailable), the retry succeeds — for component
//     tests of retries and refund branches.
//
// Internal state is a map behind a mutex; the adapter is safe for
// concurrent use.
type FakePSP struct {
	mode string

	mu         sync.Mutex
	charges    map[string]invoice.PaymentReference
	failedOnce map[string]bool
	refunded   map[string]bool
}

// NewFakePSP builds the adapter for one of PSPModeSuccess /
// PSPModeDecline / PSPModeFlaky (the PSP_MODE env value, passed in by
// the composition root — the adapter never reads the environment).
func NewFakePSP(mode string) (*FakePSP, error) {
	switch mode {
	case PSPModeSuccess, PSPModeDecline, PSPModeFlaky:
	default:
		return nil, fmt.Errorf("psp: unknown PSP_MODE %q (want %q, %q or %q)",
			mode, PSPModeSuccess, PSPModeDecline, PSPModeFlaky)
	}
	return &FakePSP{
		mode:       mode,
		charges:    map[string]invoice.PaymentReference{},
		failedOnce: map[string]bool{},
		refunded:   map[string]bool{},
	}, nil
}

func (p *FakePSP) Charge(_ context.Context, key command.ChargeKey, _ invoice.Money) (invoice.PaymentReference, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key.String()
	if ref, ok := p.charges[k]; ok {
		return ref, nil // idempotent: same key → same reference, no double debit
	}

	switch p.mode {
	case PSPModeDecline:
		return invoice.PaymentReference{}, fmt.Errorf("psp refused the charge: %w", invoice.ErrPaymentDeclined)
	case PSPModeFlaky:
		if !p.failedOnce[k] {
			p.failedOnce[k] = true
			return invoice.PaymentReference{}, fmt.Errorf("psp network error: %w", invoice.ErrPSPUnavailable)
		}
	}

	ref, err := invoice.NewPaymentReference("psp-" + uuid.NewString())
	if err != nil {
		return invoice.PaymentReference{}, fmt.Errorf("psp: build reference: %w", err)
	}
	p.charges[k] = ref
	return ref, nil
}

func (p *FakePSP) Refund(_ context.Context, key command.ChargeKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key.String()
	if _, ok := p.charges[k]; !ok {
		return nil // no charge for this key — no-op by contract
	}
	p.refunded[k] = true // idempotent: repeating a refund changes nothing
	return nil
}

// HasCharge reports whether a charge exists for the key (test/ops introspection).
func (p *FakePSP) HasCharge(key command.ChargeKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.charges[key.String()]
	return ok
}

// IsRefunded reports whether the charge for the key was refunded.
func (p *FakePSP) IsRefunded(key command.ChargeKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refunded[key.String()]
}
