package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"molot/internal/auction/app/query"
	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// AuctionPostgresReadModels implements every query-side interface of
// the context (§5) over the same Postgres: the catalog and dashboard
// from their projections, the card and the bid history directly from
// the write tables (documented decision — not every query needs a
// read model). It returns app slug errors: the query side has no
// domain to speak of.
type AuctionPostgresReadModels struct {
	db *sql.DB
}

func NewAuctionPostgresReadModels(db *sql.DB) *AuctionPostgresReadModels {
	if db == nil {
		panic("NewAuctionPostgresReadModels: nil db")
	}
	return &AuctionPostgresReadModels{db: db}
}

// ActiveAuctions serves the public catalog. EndingSoon is computed at
// read time (ends_at - now() < 5 min) — never stored (§5).
func (m *AuctionPostgresReadModels) ActiveAuctions(ctx context.Context, page, pageSize int) (query.CatalogPage, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT auction_id, title, seller_id, currency,
		       current_price_minor, minimal_next_bid_minor, bid_count, ends_at,
		       (ends_at - now()) < interval '5 minutes' AS ending_soon
		FROM auction.catalog_items
		ORDER BY ends_at, auction_id
		LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
	if err != nil {
		return query.CatalogPage{}, fmt.Errorf("unable to query catalog: %w", err)
	}
	defer rows.Close()

	items := make([]query.CatalogItem, 0, pageSize)
	for rows.Next() {
		var (
			item                query.CatalogItem
			auctionID, sellerID uuid.UUID
		)
		if err := rows.Scan(&auctionID, &item.Title, &sellerID, &item.Currency,
			&item.CurrentPriceMinor, &item.MinimalNextBidMinor, &item.BidCount,
			&item.EndsAt, &item.EndingSoon); err != nil {
			return query.CatalogPage{}, fmt.Errorf("unable to scan catalog item: %w", err)
		}
		item.AuctionID = auctionID.String()
		item.SellerID = sellerID.String()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return query.CatalogPage{}, fmt.Errorf("unable to iterate catalog: %w", err)
	}

	var total int
	if err := m.db.QueryRowContext(ctx,
		`SELECT count(*) FROM auction.catalog_items`).Scan(&total); err != nil {
		return query.CatalogPage{}, fmt.Errorf("unable to count catalog: %w", err)
	}
	return query.CatalogPage{Items: items, Total: total, Page: page}, nil
}

// AuctionCard reads the public lot page directly from the write
// tables. The reserve amount never leaves — only HasReserve.
func (m *AuctionPostgresReadModels) AuctionCard(ctx context.Context, id auction.AuctionID) (query.AuctionCardView, error) {
	var (
		view               query.AuctionCardView
		auctionID, seller  uuid.UUID
		outcome            sql.NullString
		reserveMinor       sql.NullInt64
		leadingBidder      uuid.NullUUID
		leadingAmountMinor sql.NullInt64
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT id, title, description, seller_id, status, outcome,
		       start_price_minor, increment_minor, reserve_price_minor, currency,
		       starts_at, ends_at, extensions_used, bid_count,
		       leading_bidder_id, leading_amount_minor
		FROM auction.auctions WHERE id = $1`, id.UUID()).Scan(
		&auctionID, &view.Title, &view.Description, &seller, &view.Status, &outcome,
		&view.StartPriceMinor, &view.IncrementMinor, &reserveMinor, &view.Currency,
		&view.StartsAt, &view.EndsAt, &view.ExtensionsUsed, &view.BidCount,
		&leadingBidder, &leadingAmountMinor,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return query.AuctionCardView{}, errs.NewNotFoundError("auction-not-found").WithCause(err)
	}
	if err != nil {
		return query.AuctionCardView{}, fmt.Errorf("unable to read auction card: %w", err)
	}

	view.AuctionID = auctionID.String()
	view.SellerID = seller.String()
	view.Outcome = outcome.String
	view.HasReserve = reserveMinor.Valid
	view.CurrentPriceMinor = view.StartPriceMinor
	view.MinimalNextBidMinor = view.StartPriceMinor
	if leadingAmountMinor.Valid {
		view.CurrentPriceMinor = leadingAmountMinor.Int64
		view.MinimalNextBidMinor = leadingAmountMinor.Int64 + view.IncrementMinor
	}
	if leadingBidder.Valid {
		view.LeaderID = leadingBidder.UUID.String()
	}

	recent, _, err := m.bidViews(ctx, id, 10, 0)
	if err != nil {
		return query.AuctionCardView{}, err
	}
	view.RecentBids = recent
	return view, nil
}

// Bids pages through the append-only history (newest first).
func (m *AuctionPostgresReadModels) Bids(ctx context.Context, id auction.AuctionID, page, pageSize int) (query.BidHistoryPage, error) {
	items, total, err := m.bidViews(ctx, id, pageSize, (page-1)*pageSize)
	if err != nil {
		return query.BidHistoryPage{}, err
	}
	return query.BidHistoryPage{Items: items, Total: total, Page: page}, nil
}

func (m *AuctionPostgresReadModels) bidViews(ctx context.Context, id auction.AuctionID, limit, offset int) ([]query.BidView, int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.bid_id, COALESCE(p.display_name, ''), b.amount_minor, b.placed_at
		FROM auction.auction_bids b
		LEFT JOIN auction.bidder_profiles p ON p.bidder_id = b.bidder_id
		WHERE b.auction_id = $1
		ORDER BY b.placed_at DESC, b.bid_id
		LIMIT $2 OFFSET $3`, id.UUID(), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to query bid history: %w", err)
	}
	defer rows.Close()

	items := make([]query.BidView, 0, limit)
	for rows.Next() {
		var (
			item  query.BidView
			bidID uuid.UUID
		)
		if err := rows.Scan(&bidID, &item.BidderDisplayName, &item.AmountMinor, &item.PlacedAt); err != nil {
			return nil, 0, fmt.Errorf("unable to scan bid view: %w", err)
		}
		item.BidID = bidID.String()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("unable to iterate bid history: %w", err)
	}

	var total int
	if err := m.db.QueryRowContext(ctx,
		`SELECT count(*) FROM auction.auction_bids WHERE auction_id = $1`, id.UUID()).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("unable to count bid history: %w", err)
	}
	return items, total, nil
}

// Dashboard reads the seller's projection. SoldTotalMinor is a sound
// single-currency sum: the platform currency is unique (§1).
func (m *AuctionPostgresReadModels) Dashboard(ctx context.Context, seller auction.SellerID) (query.DashboardView, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT auction_id, COALESCE(title, ''), COALESCE(status, ''), COALESCE(outcome, ''),
		       COALESCE(settlement_status, ''), COALESCE(hammer_minor, 0),
		       COALESCE(bid_count, 0), ends_at, COALESCE(currency, '')
		FROM auction.seller_dashboard_items
		WHERE seller_id = $1
		ORDER BY ends_at DESC, auction_id`, seller.UUID())
	if err != nil {
		return query.DashboardView{}, fmt.Errorf("unable to query dashboard: %w", err)
	}
	defer rows.Close()

	view := query.DashboardView{Items: []query.DashboardItem{}}
	for rows.Next() {
		var (
			item      query.DashboardItem
			auctionID uuid.UUID
			currency  string
			endsAt    sql.NullTime
		)
		if err := rows.Scan(&auctionID, &item.Title, &item.Status, &item.Outcome,
			&item.SettlementStatus, &item.HammerPriceMinor, &item.BidCount, &endsAt, &currency); err != nil {
			return query.DashboardView{}, fmt.Errorf("unable to scan dashboard item: %w", err)
		}
		item.AuctionID = auctionID.String()
		item.EndsAt = endsAt.Time
		view.Items = append(view.Items, item)

		if item.Status == "listed" {
			view.ActiveCount++
		}
		if item.Outcome == "sold" {
			view.SoldTotalMinor += item.HammerPriceMinor
		}
		if view.Currency == "" && currency != "" {
			view.Currency = currency
		}
	}
	if err := rows.Err(); err != nil {
		return query.DashboardView{}, fmt.Errorf("unable to iterate dashboard: %w", err)
	}
	return view, nil
}
