package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"molot/internal/auction/domain/auction"
)

// BidderProfilesPostgres is both sides of the bidder_profiles
// projection: the upsert fed by participant integration events and the
// read used by PlaceBid (consumer-side bidderProfiles interface in
// app/command).
//
// The two event handlers have independent consumer groups, so
// Registered and Verified may apply in any order — both writes are
// order-insensitive upserts that converge.
type BidderProfilesPostgres struct {
	db *sql.DB
}

func NewBidderProfilesPostgres(db *sql.DB) *BidderProfilesPostgres {
	if db == nil {
		panic("NewBidderProfilesPostgres: nil db")
	}
	return &BidderProfilesPostgres{db: db}
}

// UpsertRegistered records the display name; it never touches
// `verified`, so a redelivery cannot undo a verification.
func (p *BidderProfilesPostgres) UpsertRegistered(ctx context.Context, bidderID uuid.UUID, displayName string) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO auction.bidder_profiles (bidder_id, display_name, verified, updated_at)
		VALUES ($1, $2, false, now())
		ON CONFLICT (bidder_id) DO UPDATE
		SET display_name = excluded.display_name, updated_at = now()`,
		bidderID, displayName)
	if err != nil {
		return fmt.Errorf("unable to upsert bidder profile: %w", err)
	}
	return nil
}

// MarkVerified flips `verified`; if the Registered event has not been
// projected yet it creates a stub row the registration upsert will
// later fill in.
func (p *BidderProfilesPostgres) MarkVerified(ctx context.Context, bidderID uuid.UUID) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO auction.bidder_profiles (bidder_id, display_name, verified, updated_at)
		VALUES ($1, '', true, now())
		ON CONFLICT (bidder_id) DO UPDATE
		SET verified = true, updated_at = now()`,
		bidderID)
	if err != nil {
		return fmt.Errorf("unable to mark bidder verified: %w", err)
	}
	return nil
}

// BidderByID hydrates the domain Bidder for PlaceBid. Contract with
// the app layer: a bidder without a profile row is an unverified
// bidder (the projection is eventually consistent; the verification
// guard itself lives in the domain).
func (p *BidderProfilesPostgres) BidderByID(ctx context.Context, id auction.BidderID) (auction.Bidder, error) {
	var verified bool
	err := p.db.QueryRowContext(ctx,
		`SELECT verified FROM auction.bidder_profiles WHERE bidder_id = $1`, id.UUID()).Scan(&verified)
	if errors.Is(err, sql.ErrNoRows) {
		return auction.NewBidder(id, false)
	}
	if err != nil {
		return auction.Bidder{}, fmt.Errorf("unable to read bidder profile: %w", err)
	}
	return auction.NewBidder(id, verified)
}
