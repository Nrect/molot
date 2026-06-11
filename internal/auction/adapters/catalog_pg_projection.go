package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// errProjectionLag marks "the row this event updates is not projected
// yet" — a consequence of independent consumer-group offsets. The
// handler returns it so Watermill retries; the lagging handler group
// catches up within the retry budget. It is not a poison message.
var errProjectionLag = errors.New("projection row not ready yet")

// CatalogPostgresProjection maintains auction.catalog_items from the
// context's own integration events (§5): Listed → insert, BidPlaced →
// price/count/deadline with a monotonic bid_count guard, Closed and
// Cancelled → delete. Every operation is idempotent (rule 37).
type CatalogPostgresProjection struct {
	db *sql.DB
}

func NewCatalogPostgresProjection(db *sql.DB) *CatalogPostgresProjection {
	if db == nil {
		panic("NewCatalogPostgresProjection: nil db")
	}
	return &CatalogPostgresProjection{db: db}
}

// UpsertListed inserts the catalog row. With no bids the minimal next
// bid equals the start price. DO NOTHING keeps a redelivered Listed
// from regressing a row already advanced by bids.
func (p *CatalogPostgresProjection) UpsertListed(
	ctx context.Context,
	auctionID uuid.UUID, title string, sellerID uuid.UUID,
	currency string, startPriceMinor int64, endsAt time.Time,
) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO auction.catalog_items
			(auction_id, title, seller_id, currency, current_price_minor, minimal_next_bid_minor, bid_count, ends_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5, 0, $6, now())
		ON CONFLICT (auction_id) DO NOTHING`,
		auctionID, title, sellerID, currency, startPriceMinor, endsAt)
	if err != nil {
		return fmt.Errorf("unable to upsert catalog item: %w", err)
	}
	return nil
}

// ApplyBid advances price/count/deadline. The monotonic guard
// (bid_count < incoming) makes redeliveries and out-of-order updates
// no-ops; the increment for the minimal next bid is joined from the
// context's own write table (the event does not carry it).
func (p *CatalogPostgresProjection) ApplyBid(
	ctx context.Context,
	auctionID uuid.UUID, amountMinor int64, bidCount int, newEndsAt time.Time,
) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE auction.catalog_items ci SET
			current_price_minor = $2,
			minimal_next_bid_minor = $2 + a.increment_minor,
			bid_count = $3,
			ends_at = $4,
			updated_at = now()
		FROM auction.auctions a
		WHERE ci.auction_id = $1 AND a.id = $1 AND ci.bid_count < $3`,
		auctionID, amountMinor, bidCount, newEndsAt)
	if err != nil {
		return fmt.Errorf("unable to apply bid to catalog: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("unable to read rows affected: %w", err)
	}
	if affected > 0 {
		return nil
	}
	return p.classifyMiss(ctx, auctionID, bidCount)
}

// classifyMiss decides why the guarded UPDATE hit nothing: a stale
// duplicate (ack), a legitimately removed row — the auction is no
// longer listed (ack) — or projection lag (retry).
func (p *CatalogPostgresProjection) classifyMiss(ctx context.Context, auctionID uuid.UUID, bidCount int) error {
	var current int
	err := p.db.QueryRowContext(ctx,
		`SELECT bid_count FROM auction.catalog_items WHERE auction_id = $1`, auctionID).Scan(&current)
	switch {
	case err == nil:
		if current >= bidCount {
			return nil // stale duplicate — already applied
		}
		return fmt.Errorf("catalog row went backwards for %s: %w", auctionID, errProjectionLag)
	case errors.Is(err, sql.ErrNoRows):
		// Row absent: either Listed has not been projected yet, or the
		// auction is already closed/cancelled and the row was deleted.
		var status string
		stErr := p.db.QueryRowContext(ctx,
			`SELECT status FROM auction.auctions WHERE id = $1`, auctionID).Scan(&status)
		if stErr == nil && status != "listed" {
			return nil // correctly absent
		}
		return fmt.Errorf("catalog row for %s missing: %w", auctionID, errProjectionLag)
	default:
		return fmt.Errorf("unable to classify catalog miss: %w", err)
	}
}

// Remove deletes the catalog row on close/cancel. Idempotent.
func (p *CatalogPostgresProjection) Remove(ctx context.Context, auctionID uuid.UUID) error {
	if _, err := p.db.ExecContext(ctx,
		`DELETE FROM auction.catalog_items WHERE auction_id = $1`, auctionID); err != nil {
		return fmt.Errorf("unable to remove catalog item: %w", err)
	}
	return nil
}
