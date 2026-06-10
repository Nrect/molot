package decorator_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"molot/internal/common/decorator"
)

type placeBid struct{ Amount int }

type auctionCard struct{ ID string }

type recordingCommandHandler struct {
	gotCmd placeBid
	err    error
}

func (h *recordingCommandHandler) Handle(_ context.Context, cmd placeBid) error {
	h.gotCmd = cmd
	return h.err
}

type recordingQueryHandler struct {
	gotQuery auctionCard
	result   string
	err      error
}

func (h *recordingQueryHandler) Handle(_ context.Context, q auctionCard) (string, error) {
	h.gotQuery = q
	return h.result, h.err
}

// harness owns the in-memory observability sinks behind one Decorators.
type harness struct {
	deps     *decorator.Decorators
	logBuf   *bytes.Buffer
	reader   *sdkmetric.ManualReader
	exporter *tracetest.InMemoryExporter
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	exporter := tracetest.NewInMemoryExporter()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	deps, err := decorator.NewDecorators("auction", logger, meterProvider, tracerProvider)
	require.NoError(t, err)

	return &harness{deps: deps, logBuf: logBuf, reader: reader, exporter: exporter}
}

func (h *harness) histogram(t *testing.T, metricName string) metricdata.Histogram[float64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, h.reader.Collect(context.Background(), &rm))

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == metricName {
				hist, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok, "metric %s is not a float64 histogram", metricName)
				return hist
			}
		}
	}
	t.Fatalf("metric %s not found", metricName)
	return metricdata.Histogram[float64]{}
}

func attrValue(attrs attribute.Set, key string) string {
	v, _ := attrs.Value(attribute.Key(key))
	return v.AsString()
}

func TestApplyCommandDecorators(t *testing.T) {
	handlerErr := errors.New("storage down")

	testCases := []struct {
		name       string
		err        error
		wantResult string
		wantStatus codes.Code
		wantLog    string
	}{
		{"success", nil, "ok", codes.Ok, "command handler succeeded"},
		{"failure", handlerErr, "err", codes.Error, "command handler failed"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			base := &recordingCommandHandler{err: tc.err}
			decorated := decorator.ApplyCommandDecorators[placeBid](base, h.deps)

			err := decorated.Handle(context.Background(), placeBid{Amount: 100})

			// Error passthrough and untouched command.
			if tc.err != nil {
				assert.ErrorIs(t, err, handlerErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, placeBid{Amount: 100}, base.gotCmd)

			// Span: name from the command type, status from the outcome.
			spans := h.exporter.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "commands/placeBid", spans[0].Name)
			assert.Equal(t, tc.wantStatus, spans[0].Status.Code)

			// RED metric with context/handler/result attributes.
			hist := h.histogram(t, "molot_command_duration_seconds")
			require.Len(t, hist.DataPoints, 1)
			dp := hist.DataPoints[0]
			assert.Equal(t, uint64(1), dp.Count)
			assert.Equal(t, "auction", attrValue(dp.Attributes, "context"))
			assert.Equal(t, "placeBid", attrValue(dp.Attributes, "handler"))
			assert.Equal(t, tc.wantResult, attrValue(dp.Attributes, "result"))

			// Log record carries context/handler and the outcome message.
			logged := h.logBuf.String()
			assert.Contains(t, logged, tc.wantLog)
			assert.Contains(t, logged, `"context":"auction"`)
			assert.Contains(t, logged, `"handler":"placeBid"`)
			if tc.err != nil {
				assert.Contains(t, logged, "storage down")
			}
		})
	}
}

func TestApplyQueryDecorators(t *testing.T) {
	queryErr := errors.New("projection missing")

	testCases := []struct {
		name       string
		err        error
		result     string
		wantResult string
		wantStatus codes.Code
	}{
		{"success", nil, "card-42", "ok", codes.Ok},
		{"failure", queryErr, "", "err", codes.Error},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			base := &recordingQueryHandler{result: tc.result, err: tc.err}
			decorated := decorator.ApplyQueryDecorators[auctionCard, string](base, h.deps)

			got, err := decorated.Handle(context.Background(), auctionCard{ID: "42"})

			if tc.err != nil {
				assert.ErrorIs(t, err, queryErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.result, got)
			assert.Equal(t, auctionCard{ID: "42"}, base.gotQuery)

			spans := h.exporter.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "queries/auctionCard", spans[0].Name)
			assert.Equal(t, tc.wantStatus, spans[0].Status.Code)

			hist := h.histogram(t, "molot_query_duration_seconds")
			require.Len(t, hist.DataPoints, 1)
			dp := hist.DataPoints[0]
			assert.Equal(t, "auction", attrValue(dp.Attributes, "context"))
			assert.Equal(t, "auctionCard", attrValue(dp.Attributes, "handler"))
			assert.Equal(t, tc.wantResult, attrValue(dp.Attributes, "result"))
		})
	}
}

type pointerCommandHandler struct{}

func (pointerCommandHandler) Handle(context.Context, *placeBid) error { return nil }

// The use-case name must come from the element type even when handlers
// take pointer commands.
func TestUseCaseNameDereferencesPointers(t *testing.T) {
	h := newHarness(t)
	decorated := decorator.ApplyCommandDecorators[*placeBid](pointerCommandHandler{}, h.deps)

	require.NoError(t, decorated.Handle(context.Background(), &placeBid{}))

	spans := h.exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "commands/placeBid", spans[0].Name)
}
