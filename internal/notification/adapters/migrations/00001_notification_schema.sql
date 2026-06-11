-- +goose Up
CREATE SCHEMA notification;

-- Idempotency ledger: one row per delivered notification. The PK includes
-- recipient_id because a single event may fan out to several recipients
-- (ARCHITECTURE.md §7, graft G7/D6): each (kind, recipient) gets its own
-- dedup slot, so a redelivery after a partial crash sends only the
-- missing notices.
CREATE TABLE notification.sent_notifications (
    event_id     uuid        NOT NULL,
    kind         text        NOT NULL,
    recipient_id uuid        NOT NULL,
    sent_at      timestamptz NOT NULL,
    PRIMARY KEY (event_id, kind, recipient_id)
);

-- Mini-projection participant_id -> (email, display_name), fed by
-- ParticipantRegisteredV1. Not a read model: just enough address data
-- to render an email.
CREATE TABLE notification.recipients (
    participant_id uuid PRIMARY KEY,
    email          text NOT NULL,
    display_name   text NOT NULL
);

-- Mini-projection auction_id -> seller_id, fed by AuctionListedV1 and
-- AuctionClosedV1: SaleSettledV1/SaleFailedV1 address the seller but do
-- not carry the seller id (see ../../README.md).
CREATE TABLE notification.auction_sellers (
    auction_id uuid PRIMARY KEY,
    seller_id  uuid NOT NULL
);

-- +goose Down
DROP TABLE notification.auction_sellers;
DROP TABLE notification.recipients;
DROP TABLE notification.sent_notifications;
DROP SCHEMA notification;
