package adapters

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DashboardPostgresProjection maintains auction.seller_dashboard_items
// (§5): Listed/BidPlaced/Closed/Cancelled/SaleSettled/SaleFailed/
// Relisted upsert statuses. Rows are never deleted; every operation is
// idempotent (rule 37).
type DashboardPostgresProjection struct {
	db *sql.DB
}

func NewDashboardPostgresProjection(db *sql.DB) *DashboardPostgresProjection {
	if db == nil {
		panic("NewDashboardPostgresProjection: nil db")
	}
	return &DashboardPostgresProjection{db: db}
}

func (p *DashboardPostgresProjection) UpsertListed(
	ctx context.Context,
	auctionID, sellerID uuid.UUID, title, currency string, endsAt time.Time,
) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO auction.seller_dashboard_items
			(auction_id, seller_id, title, status, outcome, settlement_status, hammer_minor, currency, bid_count, ends_at, updated_at)
		VALUES ($1, $2, $3, 'listed', NULL, NULL, NULL, $4, 0, $5, now())
		ON CONFLICT (auction_id) DO NOTHING`,
		auctionID, sellerID, title, currency, endsAt)
	if err != nil {
		return fmt.Errorf("unable to upsert dashboard item: %w", err)
	}
	return nil
}

func (p *DashboardPostgresProjection) ApplyBid(
	ctx context.Context,
	auctionID uuid.UUID, bidCount int, newEndsAt time.Time,
) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE auction.seller_dashboard_items
		SET bid_count = $2, ends_at = $3, updated_at = now()
		WHERE auction_id = $1 AND bid_count < $2`,
		auctionID, bidCount, newEndsAt)
	if err != nil {
		return fmt.Errorf("unable to apply bid to dashboard: %w", err)
	}
	return p.ackOrLag(ctx, res, auctionID, "apply bid")
}

func (p *DashboardPostgresProjection) ApplyClosed(
	ctx context.Context,
	auctionID uuid.UUID, outcome string, hammerMinor int64,
) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE auction.seller_dashboard_items
		SET status = 'closed', outcome = $2, hammer_minor = $3, updated_at = now()
		WHERE auction_id = $1`,
		auctionID, outcome, hammerMinor)
	if err != nil {
		return fmt.Errorf("unable to apply close to dashboard: %w", err)
	}
	return p.ackOrLag(ctx, res, auctionID, "apply close")
}

func (p *DashboardPostgresProjection) ApplyCancelled(ctx context.Context, auctionID uuid.UUID) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE auction.seller_dashboard_items
		SET status = 'cancelled', updated_at = now()
		WHERE auction_id = $1`, auctionID)
	if err != nil {
		return fmt.Errorf("unable to apply cancel to dashboard: %w", err)
	}
	return p.ackOrLag(ctx, res, auctionID, "apply cancel")
}

// ApplySettlement records the saga verdict; SaleFailed also flips the
// outcome to not_sold (mirroring the aggregate transition).
func (p *DashboardPostgresProjection) ApplySettlement(
	ctx context.Context,
	auctionID uuid.UUID, settlementStatus string, outcome string,
) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE auction.seller_dashboard_items
		SET settlement_status = $2, outcome = COALESCE(NULLIF($3, ''), outcome), updated_at = now()
		WHERE auction_id = $1`,
		auctionID, settlementStatus, outcome)
	if err != nil {
		return fmt.Errorf("unable to apply settlement to dashboard: %w", err)
	}
	return p.ackOrLag(ctx, res, auctionID, "apply settlement")
}

// MarkRelisted stamps the ORIGINAL auction's row; the replacement gets
// its own row via its AuctionListedV1.
func (p *DashboardPostgresProjection) MarkRelisted(ctx context.Context, originalID uuid.UUID) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE auction.seller_dashboard_items
		SET settlement_status = 'relisted', updated_at = now()
		WHERE auction_id = $1`, originalID)
	if err != nil {
		return fmt.Errorf("unable to mark dashboard item relisted: %w", err)
	}
	return p.ackOrLag(ctx, res, originalID, "mark relisted")
}

// ackOrLag: dashboard rows are never deleted, so an UPDATE that hit
// nothing means the Listed event of this auction has not been
// projected yet (independent consumer-group offsets) — retryable.
func (p *DashboardPostgresProjection) ackOrLag(ctx context.Context, res sql.Result, auctionID uuid.UUID, op string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("unable to read rows affected: %w", err)
	}
	if affected > 0 {
		return nil
	}
	var exists bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM auction.seller_dashboard_items WHERE auction_id = $1)`,
		auctionID).Scan(&exists); err != nil {
		return fmt.Errorf("unable to classify dashboard miss: %w", err)
	}
	if exists {
		return nil // guarded no-op (stale duplicate)
	}
	return fmt.Errorf("dashboard %s for %s: %w", op, auctionID, errProjectionLag)
}
