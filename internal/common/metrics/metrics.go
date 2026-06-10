// Package metrics initializes the OTel MeterProvider for the monolith:
// OTLP gRPC exporter (insecure, for the local collector from
// docker-compose), resource service.name=molot and Go runtime
// instrumentation (ARCHITECTURE.md §11, ADR-0005).
package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// NewMeterProvider builds the process MeterProvider exporting over OTLP
// gRPC to endpoint (host:port, plaintext — local collector), installs
// it as the global provider and starts Go runtime instrumentation.
//
// The caller owns the returned provider and must call Shutdown(ctx) on
// process teardown to flush pending metrics.
func NewMeterProvider(ctx context.Context, endpoint string) (*sdkmetric.MeterProvider, error) {
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}

	res, err := newResource()
	if err != nil {
		return nil, fmt.Errorf("build OTel resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(provider)

	if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
		shutdownCtx := context.WithoutCancel(ctx)
		_ = provider.Shutdown(shutdownCtx)
		return nil, fmt.Errorf("start runtime instrumentation: %w", err)
	}

	return provider, nil
}

// newResource describes this process for every exported metric.
// Deliberately duplicated in common/tracing: the two packages stay
// independently usable (same rationale as duplicated Money per context).
func newResource() (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("molot"),
		),
	)
}
