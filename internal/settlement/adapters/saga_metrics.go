package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/metric"

	"molot/internal/settlement/domain/settlement"
)

// RegisterNonterminalAgeGauge registers
// molot_settlement_nonterminal_age_seconds (§11): the age of the oldest
// saga that has not reached a terminal state — the "stuck saga" alarm
// (alert > 2×PAYMENT_TERM). It is an observable gauge: the settlements
// scan runs on every metric collection, no extra goroutine. The service
// composition root calls this once at startup.
func RegisterNonterminalAgeGauge(db *sql.DB, meterProvider metric.MeterProvider) error {
	if db == nil {
		return errors.New("settlement nonterminal age gauge: nil db")
	}
	if meterProvider == nil {
		return errors.New("settlement nonterminal age gauge: nil meter provider")
	}

	meter := meterProvider.Meter("molot/internal/settlement/adapters")
	gauge, err := meter.Float64ObservableGauge("molot_settlement_nonterminal_age_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest settlement saga not yet in a terminal state"))
	if err != nil {
		return fmt.Errorf("settlement nonterminal age gauge: %w", err)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		var age float64
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(started_at)))::float8, 0)
			FROM settlement.settlements
			WHERE state NOT IN ($1, $2, $3)`,
			settlement.StateSettled.String(),
			settlement.StateRelisted.String(),
			settlement.StateFailedUnsold.String(),
		).Scan(&age)
		if err != nil {
			return fmt.Errorf("settlement nonterminal age gauge: scan settlements: %w", err)
		}
		o.ObserveFloat64(gauge, age)
		return nil
	}, gauge)
	if err != nil {
		return fmt.Errorf("settlement nonterminal age gauge: register callback: %w", err)
	}
	return nil
}
