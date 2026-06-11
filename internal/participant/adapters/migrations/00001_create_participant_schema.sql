-- Participant context schema (ARCHITECTURE.md §7): one schema per
-- bounded context, cross-schema access is forbidden. Time is UTC-only
-- (timestamptz); UNIQUE(email) backs the ErrEmailTaken contract.

-- +goose Up
CREATE SCHEMA IF NOT EXISTS participant;

CREATE TABLE participant.participants (
    id           uuid PRIMARY KEY,
    email        text NOT NULL UNIQUE,
    display_name text NOT NULL,
    status       text NOT NULL,                -- registered|verified
    version      bigint NOT NULL DEFAULT 1,    -- optimistic lock on top of FOR UPDATE
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL
);

-- +goose Down
DROP TABLE participant.participants;
DROP SCHEMA participant;
