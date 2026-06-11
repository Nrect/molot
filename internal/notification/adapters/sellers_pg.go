package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SellersPG is the Postgres seller directory: the mini-projection
// auction_id → seller_id fed by AuctionListedV1/AuctionClosedV1, used
// to address SaleSettledV1/SaleFailedV1 notices that do not carry the
// seller id themselves.
type SellersPG struct {
	db *sql.DB
}

func NewSellersPG(db *sql.DB) *SellersPG {
	if db == nil {
		panic("notification.NewSellersPG: nil db")
	}
	return &SellersPG{db: db}
}

// RememberSeller is idempotent; the seller of an auction never changes,
// but DO UPDATE keeps the row self-healing on redelivery.
func (s *SellersPG) RememberSeller(ctx context.Context, auctionID, sellerID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification.auction_sellers (auction_id, seller_id)
		VALUES ($1, $2)
		ON CONFLICT (auction_id)
		DO UPDATE SET seller_id = excluded.seller_id`,
		auctionID, sellerID)
	if err != nil {
		return fmt.Errorf("unable to remember seller of auction %s: %w", auctionID, err)
	}
	return nil
}

// SellerOf resolves the seller of an auction. An unknown auction is
// projection lag — the error makes the bus redeliver until the
// listing/closing event is consumed.
func (s *SellersPG) SellerOf(ctx context.Context, auctionID string) (string, error) {
	var sellerID string
	err := s.db.QueryRowContext(ctx,
		`SELECT seller_id FROM notification.auction_sellers WHERE auction_id = $1`,
		auctionID).Scan(&sellerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("seller of auction %s is not known yet", auctionID)
	}
	if err != nil {
		return "", fmt.Errorf("unable to query seller of auction %s: %w", auctionID, err)
	}
	return sellerID, nil
}
