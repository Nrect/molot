-- Billing schema (ARCHITECTURE.md §7): one schema per context,
-- cross-schema queries are forbidden. All time is timestamptz (UTC).

-- +goose Up
CREATE SCHEMA IF NOT EXISTS billing;

CREATE TABLE billing.invoices (
    id               uuid PRIMARY KEY,
    auction_id       uuid NOT NULL,
    debtor_id        uuid NOT NULL,
    hammer_minor     bigint NOT NULL,
    commission_minor bigint NOT NULL,
    total_minor      bigint NOT NULL,
    currency         char(3) NOT NULL,
    status           text NOT NULL,            -- pending|paid|expired|voided
    due_at           timestamptz NOT NULL,
    attempt          int NOT NULL,             -- 1 | 2 (second chance)
    psp_ref          text NULL,                -- NULL until paid
    version          bigint NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    UNIQUE (auction_id, attempt)               -- second line of IssueInvoice idempotency
);

-- Partial index for the expiry worker's due scan (§10).
CREATE INDEX invoices_due_idx ON billing.invoices (due_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE billing.invoices;
DROP SCHEMA IF EXISTS billing;
