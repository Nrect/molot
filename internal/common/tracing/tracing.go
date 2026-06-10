// Package tracing initializes the OTel TracerProvider for the
// monolith: OTLP gRPC exporter (insecure, for the local collector from
// docker-compose), resource service.name=molot and W3C propagation
// (traceparent + baggage) so the trace survives HTTP and message-bus
// hops (ARCHITECTURE.md §11, ADR-0005).
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// NewTracerProvider builds the process TracerProvider exporting over
// OTLP gRPC to endpoint (host:port, plaintext — local collector),
// installs it as the global provider and registers the W3C composite
// propagator (TraceContext + Baggage).
//
// The caller owns the returned provider and must call Shutdown(ctx) on
// process teardown to flush pending spans.
func NewTracerProvider(ctx context.Context, endpoint string) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	res, err := newResource()
	if err != nil {
		return nil, fmt.Errorf("build OTel resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return provider, nil
}

// newResource describes this process for every exported span.
// Deliberately duplicated in common/metrics: the two packages stay
// independently usable.
func newResource() (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("molot"),
		),
	)
}
