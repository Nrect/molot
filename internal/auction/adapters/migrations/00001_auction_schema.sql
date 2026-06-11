-- +goose Up
-- Auction context schema (ARCHITECTURE §7). Schema-per-context;
-- cross-schema queries are forbidden by review + CI. All time is UTC
-- (timestamptz).
CREATE SCHEMA IF NOT EXISTS auction;

CREATE TABLE auction.auctions (
    id                  uuid PRIMARY KEY,
    seller_id           uuid        NOT NULL,
    title               text        NOT NULL,
    description         text        NOT NULL,
    currency            char(3)     NOT NULL,
    start_price_minor   bigint      NOT NULL,
    increment_minor     bigint      NOT NULL,
    reserve_price_minor bigint      NULL,            -- NULL = no reserve (domain: ReservePrice.IsZero)
    starts_at           timestamptz NOT NULL,
    ends_at             timestamptz NOT NULL,
    original_ends_at    timestamptz NOT NULL,
    snipe_window_sec    int         NOT NULL,        -- anti-snipe policy snapshot
    snipe_extension_sec int         NOT NULL,
    snipe_max_ext       int         NOT NULL,
    verify_above_minor  bigint      NOT NULL,        -- verified_bid_threshold snapshot at listing
    extensions_used     int         NOT NULL DEFAULT 0,
    status              text        NOT NULL,        -- listed|cancelled|closed
    outcome             text        NULL,            -- sold|not_sold
    -- top-2 bids denormalized on the aggregate row (ADR-0003); the
    -- runner-up keeps full bid identity so it can be promoted by the
    -- saga without consulting the history log
    leading_bid_id         uuid        NULL,
    leading_bidder_id      uuid        NULL,
    leading_amount_minor   bigint      NULL,
    leading_placed_at      timestamptz NULL,
    runner_up_bid_id       uuid        NULL,
    runner_up_bidder_id    uuid        NULL,
    runner_up_amount_minor bigint      NULL,
    runner_up_placed_at    timestamptz NULL,
    winner_reassigned   bool        NOT NULL DEFAULT false,
    bid_count           int         NOT NULL DEFAULT 0,
    relist_of           uuid        NULL,
    relist_generation   int         NOT NULL DEFAULT 0,
    settled             bool        NOT NULL DEFAULT false,
    version             bigint      NOT NULL DEFAULT 1,  -- optimistic lock on top of FOR UPDATE
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL
);

-- closing worker candidate scan
CREATE INDEX auctions_due_idx ON auction.auctions (ends_at) WHERE status = 'listed';
-- second line of relist idempotency on top of the deterministic uuidv5 new id
CREATE UNIQUE INDEX auctions_relist_of_uq ON auction.auctions (relist_of) WHERE relist_of IS NOT NULL;

-- append-only bid log: inside the aggregate's consistency boundary,
-- written in the same transaction, never UPDATEd, never loaded into
-- the aggregate
CREATE TABLE auction.auction_bids (
    bid_id       uuid PRIMARY KEY,
    auction_id   uuid        NOT NULL REFERENCES auction.auctions (id),
    bidder_id    uuid        NOT NULL,
    amount_minor bigint      NOT NULL,
    currency     char(3)     NOT NULL,
    placed_at    timestamptz NOT NULL
);
CREATE INDEX bids_history_idx ON auction.auction_bids (auction_id, placed_at DESC);

-- projection of participant events: only the fields this context needs
CREATE TABLE auction.bidder_profiles (
    bidder_id    uuid PRIMARY KEY,
    display_name text        NOT NULL,
    verified     bool        NOT NULL,
    updated_at   timestamptz NOT NULL
);

-- catalog read projection; ending_soon is NOT stored (computed at read
-- time — it would go stale between events)
CREATE TABLE auction.catalog_items (
    auction_id             uuid PRIMARY KEY,
    title                  text,
    seller_id              uuid,
    currency               char(3),
    current_price_minor    bigint,
    minimal_next_bid_minor bigint,
    bid_count              int,
    ends_at                timestamptz,
    updated_at             timestamptz
);
CREATE INDEX catalog_ends_idx ON auction.catalog_items (ends_at);

-- seller dashboard read projection
CREATE TABLE auction.seller_dashboard_items (
    auction_id        uuid PRIMARY KEY,
    seller_id         uuid NOT NULL,
    title             text,
    status            text,
    outcome           text,
    settlement_status text,
    hammer_minor      bigint,
    currency          char(3),
    bid_count         int,
    ends_at           timestamptz,
    updated_at        timestamptz
);
CREATE INDEX dashboard_seller_idx ON auction.seller_dashboard_items (seller_id);

-- +goose Down
DROP SCHEMA auction CASCADE;
