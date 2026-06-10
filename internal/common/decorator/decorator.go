// Package decorator applies cross-cutting concerns (logging, RED
// metrics, tracing) to every command and query handler, so the app
// layer stays free of observability plumbing (ARCHITECTURE.md §11,
// ADR-0005).
//
// Each bounded context builds one *Decorators value (bound to the
// context module name) and wraps every handler with
// ApplyCommandDecorators / ApplyQueryDecorators. The use-case name is
// derived from the command/query type name via reflection, so traces
// read as a catalog of app.Application: "commands/PlaceBid",
// "queries/AuctionCard".
package decorator

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// CommandHandler is the shape of every command use case in the app layer.
type CommandHandler[C any] interface {
	Handle(ctx context.Context, cmd C) error
}

// QueryHandler is the shape of every query use case in the app layer.
type QueryHandler[Q any, R any] interface {
	Handle(ctx context.Context, query Q) (R, error)
}

const (
	commandDurationMetric = "molot_command_duration_seconds"
	queryDurationMetric   = "molot_query_duration_seconds"

	instrumentationName = "molot/internal/common/decorator"
)

// Decorators carries the cross-cutting dependencies of one context
// module. Build it once per bounded context and reuse it for every
// handler of that context.
type Decorators struct {
	contextName string
	logger      *slog.Logger
	tracer      trace.Tracer

	commandDuration metric.Float64Histogram
	queryDuration   metric.Float64Histogram
}

// NewDecorators binds the decorator stack to a context module
// (e.g. "auction") and to the observability providers.
func NewDecorators(
	contextName string,
	logger *slog.Logger,
	meterProvider metric.MeterProvider,
	tracerProvider trace.TracerProvider,
) (*Decorators, error) {
	meter := meterProvider.Meter(instrumentationName)

	commandDuration, err := meter.Float64Histogram(
		commandDurationMetric,
		metric.WithDescription("Duration of command handler executions"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", commandDurationMetric, err)
	}

	queryDuration, err := meter.Float64Histogram(
		queryDurationMetric,
		metric.WithDescription("Duration of query handler executions"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", queryDurationMetric, err)
	}

	return &Decorators{
		contextName:     contextName,
		logger:          logger,
		tracer:          tracerProvider.Tracer(instrumentationName),
		commandDuration: commandDuration,
		queryDuration:   queryDuration,
	}, nil
}

// ApplyCommandDecorators wraps handler with tracing (outermost, so log
// records and metrics observe the span context), logging and metrics
// (innermost, measuring pure handler time).
func ApplyCommandDecorators[C any](handler CommandHandler[C], d *Decorators) CommandHandler[C] {
	name := useCaseName[C]()
	return commandTracingDecorator[C]{
		base: commandLoggingDecorator[C]{
			base: commandMetricsDecorator[C]{base: handler, deps: d, name: name},
			deps: d,
			name: name,
		},
		deps: d,
		name: name,
	}
}

// ApplyQueryDecorators is ApplyCommandDecorators for queries.
func ApplyQueryDecorators[Q any, R any](handler QueryHandler[Q, R], d *Decorators) QueryHandler[Q, R] {
	name := useCaseName[Q]()
	return queryTracingDecorator[Q, R]{
		base: queryLoggingDecorator[Q, R]{
			base: queryMetricsDecorator[Q, R]{base: handler, deps: d, name: name},
			deps: d,
			name: name,
		},
		deps: d,
		name: name,
	}
}

// useCaseName extracts the bare type name of the command/query
// ("PlaceBid" from command.PlaceBid or *command.PlaceBid).
func useCaseName[T any]() string {
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if name := t.Name(); name != "" {
		return name
	}
	return t.String()
}

// --- tracing -------------------------------------------------------------

type commandTracingDecorator[C any] struct {
	base CommandHandler[C]
	deps *Decorators
	name string
}

func (d commandTracingDecorator[C]) Handle(ctx context.Context, cmd C) (err error) {
	ctx, span := d.deps.tracer.Start(ctx, "commands/"+d.name)
	defer span.End()

	err = d.base.Handle(ctx, cmd)
	recordSpanResult(span, err)
	return err
}

type queryTracingDecorator[Q any, R any] struct {
	base QueryHandler[Q, R]
	deps *Decorators
	name string
}

func (d queryTracingDecorator[Q, R]) Handle(ctx context.Context, query Q) (result R, err error) {
	ctx, span := d.deps.tracer.Start(ctx, "queries/"+d.name)
	defer span.End()

	result, err = d.base.Handle(ctx, query)
	recordSpanResult(span, err)
	return result, err
}

func recordSpanResult(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetStatus(codes.Ok, "")
}

// --- logging -------------------------------------------------------------

type commandLoggingDecorator[C any] struct {
	base CommandHandler[C]
	deps *Decorators
	name string
}

func (d commandLoggingDecorator[C]) Handle(ctx context.Context, cmd C) (err error) {
	logger := handlerLogger(d.deps, d.name)
	defer func() {
		if err != nil {
			logger.ErrorContext(ctx, "command handler failed", slog.Any("error", err))
			return
		}
		logger.InfoContext(ctx, "command handler succeeded")
	}()

	return d.base.Handle(ctx, cmd)
}

type queryLoggingDecorator[Q any, R any] struct {
	base QueryHandler[Q, R]
	deps *Decorators
	name string
}

func (d queryLoggingDecorator[Q, R]) Handle(ctx context.Context, query Q) (result R, err error) {
	logger := handlerLogger(d.deps, d.name)
	defer func() {
		if err != nil {
			logger.ErrorContext(ctx, "query handler failed", slog.Any("error", err))
			return
		}
		logger.InfoContext(ctx, "query handler succeeded")
	}()

	return d.base.Handle(ctx, query)
}

func handlerLogger(d *Decorators, name string) *slog.Logger {
	return d.logger.With(
		slog.String("context", d.contextName),
		slog.String("handler", name),
	)
}

// --- metrics (RED) -------------------------------------------------------

type commandMetricsDecorator[C any] struct {
	base CommandHandler[C]
	deps *Decorators
	name string
}

func (d commandMetricsDecorator[C]) Handle(ctx context.Context, cmd C) (err error) {
	start := time.Now()
	defer func() {
		d.deps.commandDuration.Record(
			ctx,
			time.Since(start).Seconds(),
			metric.WithAttributes(durationAttributes(d.deps.contextName, d.name, err)...),
		)
	}()

	return d.base.Handle(ctx, cmd)
}

type queryMetricsDecorator[Q any, R any] struct {
	base QueryHandler[Q, R]
	deps *Decorators
	name string
}

func (d queryMetricsDecorator[Q, R]) Handle(ctx context.Context, query Q) (result R, err error) {
	start := time.Now()
	defer func() {
		d.deps.queryDuration.Record(
			ctx,
			time.Since(start).Seconds(),
			metric.WithAttributes(durationAttributes(d.deps.contextName, d.name, err)...),
		)
	}()

	return d.base.Handle(ctx, query)
}

func durationAttributes(contextName, handlerName string, err error) []attribute.KeyValue {
	result := "ok"
	if err != nil {
		result = "err"
	}
	return []attribute.KeyValue{
		attribute.String("context", contextName),
		attribute.String("handler", handlerName),
		attribute.String("result", result),
	}
}
