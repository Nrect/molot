-- Settlement schema (ARCHITECTURE.md §7): one schema per context,
-- cross-schema queries are forbidden. All time is timestamptz (UTC).

-- +goose Up
CREATE SCHEMA IF NOT EXISTS settlement;

CREATE TABLE settlement.settlements (
    auction_id          uuid PRIMARY KEY,          -- one saga per auction; the natural idempotency key (§6.6)
    state               text NOT NULL,             -- started|awaiting_payment|awarding_runner_up|second_chance_payment|settled|relisted|failed_unsold
    winner_id           uuid NOT NULL,             -- current debtor (the runner-up after an award)
    hammer_minor        bigint NOT NULL,
    currency            char(3) NOT NULL,
    runner_up_id        uuid NULL,
    runner_up_minor     bigint NULL,
    runner_up_qualifies bool NOT NULL,
    relist_generation   int NOT NULL,
    attempt             int NOT NULL,              -- 1 | 2 (second chance)
    invoice_id          uuid NULL,                 -- the invoice the saga currently awaits
    failure_reason      text NULL,                 -- payment_timeout|second_chance_declined
    version             bigint NOT NULL DEFAULT 1,
    started_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL
);

-- +goose Down
DROP TABLE settlement.settlements;
DROP SCHEMA IF EXISTS settlement;
