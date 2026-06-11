// Bus health gauges of the observability map (§11): dead-letter size
// and per-topic consumer lag, both computed by scanning the watermill
// tables. They are observable gauges — the scan runs on every metric
// collection (the periodic reader interval), no extra goroutine.
package watermill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RegisterBusMetrics registers molot_bus_dead_letter_size and
// molot_bus_oldest_message_age_seconds{topic} on the meter provider.
//
// Both gauges use the "slowest consumer group" view of a topic: a
// message counts as pending while at least one consumer group has not
// acked past it (offset > min(offset_acked)); a topic without offsets
// rows (no subscriber yet — e.g. the dead letter before the redelivery
// runbook runs) counts every message. Topics are discovered from
// information_schema on each observation, so gauges appear as soon as
// the first publisher/subscriber initializes a topic's schema.
func RegisterBusMetrics(db *sql.DB, meterProvider metric.MeterProvider) error {
	if db == nil {
		return errors.New("watermill bus metrics: nil db")
	}
	if meterProvider == nil {
		return errors.New("watermill bus metrics: nil meter provider")
	}

	meter := meterProvider.Meter("molot/internal/common/watermill")

	deadLetterSize, err := meter.Int64ObservableGauge("molot_bus_dead_letter_size",
		metric.WithDescription("Messages sitting in the dead-letter topic, not yet redelivered (alert > 0, §6.8)"))
	if err != nil {
		return fmt.Errorf("watermill bus metrics: dead letter gauge: %w", err)
	}
	oldestAge, err := meter.Float64ObservableGauge("molot_bus_oldest_message_age_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest message not yet acked by the slowest consumer group, per topic"))
	if err != nil {
		return fmt.Errorf("watermill bus metrics: oldest age gauge: %w", err)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		topics, err := watermillTopics(ctx, db)
		if err != nil {
			return fmt.Errorf("watermill bus metrics: discover topics: %w", err)
		}

		// The dead letter is observed even before its schema exists:
		// "no table yet" and "empty" both mean a healthy zero.
		var deadLetterPending int64

		for _, t := range topics {
			pending, age, err := topicBacklog(ctx, db, t)
			if err != nil {
				return fmt.Errorf("watermill bus metrics: scan topic %q: %w", t.name, err)
			}
			o.ObserveFloat64(oldestAge, age, metric.WithAttributes(attribute.String("topic", t.name)))
			if t.name == DeadLetterTopic {
				deadLetterPending = pending
			}
		}

		o.ObserveInt64(deadLetterSize, deadLetterPending)
		return nil
	}, deadLetterSize, oldestAge)
	if err != nil {
		return fmt.Errorf("watermill bus metrics: register callback: %w", err)
	}
	return nil
}

// busTopic is one discovered watermill topic with its physical tables.
type busTopic struct {
	name          string
	messagesTable string
	offsetsTable  string // empty when no subscriber initialized offsets yet
}

const (
	messagesTablePrefix = "watermill_"
	offsetsTablePrefix  = "watermill_offsets_"
)

// watermillTopics lists every initialized topic by scanning
// information_schema for the DefaultPostgreSQLSchema table layout
// (watermill_<topic> + watermill_offsets_<topic>).
func watermillTopics(ctx context.Context, db *sql.DB) ([]busTopic, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name LIKE 'watermill\_%'
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []string
	offsets := map[string]bool{}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		if strings.HasPrefix(table, offsetsTablePrefix) {
			offsets[strings.TrimPrefix(table, offsetsTablePrefix)] = true
			continue
		}
		messages = append(messages, table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	topics := make([]busTopic, 0, len(messages))
	for _, table := range messages {
		topic := strings.TrimPrefix(table, messagesTablePrefix)
		t := busTopic{name: topic, messagesTable: table}
		if offsets[topic] {
			t.offsetsTable = offsetsTablePrefix + topic
		}
		topics = append(topics, t)
	}
	return topics, nil
}

// topicBacklog returns how many messages of the topic are still pending
// for the slowest consumer group and the age of the oldest of them.
func topicBacklog(ctx context.Context, db *sql.DB, t busTopic) (pending int64, ageSeconds float64, err error) {
	var ackedFloor int64
	if t.offsetsTable != "" {
		err := db.QueryRowContext(ctx,
			`SELECT COALESCE(MIN(offset_acked), 0) FROM `+quoteIdent(t.offsetsTable)).Scan(&ackedFloor)
		if err != nil {
			return 0, 0, err
		}
	}

	// created_at is TIMESTAMP (no tz) filled by the DB's
	// CURRENT_TIMESTAMP, so the age is computed against now() in the
	// same session time zone.
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(EXTRACT(EPOCH FROM (now()::timestamp - MIN(created_at)))::float8, 0)
		FROM `+quoteIdent(t.messagesTable)+`
		WHERE "offset" > $1`, ackedFloor).Scan(&pending, &ageSeconds)
	if err != nil {
		return 0, 0, err
	}
	return pending, ageSeconds, nil
}

// quoteIdent quotes a Postgres identifier (topic names may contain
// dots — the dead letter table is "watermill_events.dead_letter").
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
