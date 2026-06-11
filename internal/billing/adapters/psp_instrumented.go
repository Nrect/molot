package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"molot/internal/billing/app/command"
	"molot/internal/billing/domain/invoice"
)

const (
	pspInstrumentationName = "molot/internal/billing/adapters"

	pspCallsMetric    = "molot_psp_calls_total"
	pspDurationMetric = "molot_psp_call_duration_seconds"

	attrOp     = attribute.Key("op")
	attrResult = attribute.Key("result")

	opCharge = "charge"
	opRefund = "refund"

	resultSuccess   = "success"
	resultDeclined  = "declined"
	resultUnavail   = "unavailable"
	resultError     = "error"
)

// pspGateway mirrors the consumer-side interface from the command package
// without importing it (the decorator lives in the same package as its
// dependants and the interface is unexported in command — we redeclare the
// identical shape here so the adapters package can own the decorator).
//
// The interface is satisfied by *FakePSP and by any real PSP adapter.
type pspGateway interface {
	Charge(ctx context.Context, key command.ChargeKey, amount invoice.Money) (invoice.PaymentReference, error)
	Refund(ctx context.Context, key command.ChargeKey) error
}

// InstrumentedPSP wraps any pspGateway with OTel metrics and tracing:
//
//   - Counter  molot_psp_calls_total{op, result}
//   - Histogram molot_psp_call_duration_seconds{op, result}
//   - Span "psp.Charge" / "psp.Refund" with attribute idempotency_key
//
// result is classified as:
//
//	success     — no error
//	declined    — errors.Is(err, invoice.ErrPaymentDeclined)
//	unavailable — errors.Is(err, invoice.ErrPSPUnavailable)
//	error       — any other non-nil error
type InstrumentedPSP struct {
	next   pspGateway
	tracer trace.Tracer

	calls    metric.Int64Counter
	duration metric.Float64Histogram
}

// NewInstrumentedPSP builds the decorator.  meterProvider and
// tracerProvider must be non-nil (the composition root guarantees this;
// the constructor panics on violation, matching the service-level
// convention in §6).
func NewInstrumentedPSP(
	next pspGateway,
	meterProvider metric.MeterProvider,
	tracerProvider trace.TracerProvider,
) (*InstrumentedPSP, error) {
	if next == nil {
		panic("NewInstrumentedPSP: nil gateway")
	}
	if meterProvider == nil {
		panic("NewInstrumentedPSP: nil meter provider")
	}
	if tracerProvider == nil {
		panic("NewInstrumentedPSP: nil tracer provider")
	}

	meter := meterProvider.Meter(pspInstrumentationName)

	calls, err := meter.Int64Counter(
		pspCallsMetric,
		metric.WithDescription("Total PSP calls by operation and result"),
	)
	if err != nil {
		return nil, fmt.Errorf("psp instrumentation: create %s counter: %w", pspCallsMetric, err)
	}

	dur, err := meter.Float64Histogram(
		pspDurationMetric,
		metric.WithDescription("Duration of PSP calls in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("psp instrumentation: create %s histogram: %w", pspDurationMetric, err)
	}

	return &InstrumentedPSP{
		next:     next,
		tracer:   tracerProvider.Tracer(pspInstrumentationName),
		calls:    calls,
		duration: dur,
	}, nil
}

// Charge forwards to the wrapped gateway and records metrics + span.
func (p *InstrumentedPSP) Charge(ctx context.Context, key command.ChargeKey, amount invoice.Money) (invoice.PaymentReference, error) {
	ctx, span := p.tracer.Start(ctx, "psp.Charge",
		trace.WithAttributes(attribute.String("idempotency_key", key.String())),
	)
	defer span.End()

	start := time.Now()
	ref, err := p.next.Charge(ctx, key, amount)
	elapsed := time.Since(start)

	res := classifyResult(err)
	attrs := metric.WithAttributes(attrOp.String(opCharge), attrResult.String(res))
	p.calls.Add(ctx, 1, attrs)
	p.duration.Record(ctx, elapsed.Seconds(), attrs)

	recordPSPSpanResult(span, err)
	return ref, err
}

// Refund forwards to the wrapped gateway and records metrics + span.
func (p *InstrumentedPSP) Refund(ctx context.Context, key command.ChargeKey) error {
	ctx, span := p.tracer.Start(ctx, "psp.Refund",
		trace.WithAttributes(attribute.String("idempotency_key", key.String())),
	)
	defer span.End()

	start := time.Now()
	err := p.next.Refund(ctx, key)
	elapsed := time.Since(start)

	res := classifyResult(err)
	attrs := metric.WithAttributes(attrOp.String(opRefund), attrResult.String(res))
	p.calls.Add(ctx, 1, attrs)
	p.duration.Record(ctx, elapsed.Seconds(), attrs)

	recordPSPSpanResult(span, err)
	return err
}

// classifyResult maps an error to its metric result label.
func classifyResult(err error) string {
	if err == nil {
		return resultSuccess
	}
	if errors.Is(err, invoice.ErrPaymentDeclined) {
		return resultDeclined
	}
	if errors.Is(err, invoice.ErrPSPUnavailable) {
		return resultUnavail
	}
	return resultError
}

func recordPSPSpanResult(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetStatus(codes.Ok, "")
}
