package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"molot/internal/auction/domain/auction"
	auctionevents "molot/internal/auction/events"
	"molot/internal/common/postgres"
	cwatermill "molot/internal/common/watermill"
)

const pgUniqueViolation = "23505"

// AuctionPostgresRepository persists the Auction aggregate (§2.1, §7):
// the snapshot row in auction.auctions under SELECT ... FOR UPDATE +
// optimistic version, the append-only bid log in auction.auction_bids
// (from recorded BidPlaced events) and the integration events through
// the transactional outbox — all in one transaction.
type AuctionPostgresRepository struct {
	db       *sql.DB
	wmLogger wm.LoggerAdapter
}

func NewAuctionPostgresRepository(db *sql.DB, wmLogger wm.LoggerAdapter) *AuctionPostgresRepository {
	if db == nil {
		panic("NewAuctionPostgresRepository: nil db")
	}
	if wmLogger == nil {
		panic("NewAuctionPostgresRepository: nil watermill logger")
	}
	return &AuctionPostgresRepository{db: db, wmLogger: wmLogger}
}

// pgAuction is the storage model — never shared with domain or
// transport (rule 7); the domain is rebuilt only through
// auction.UnmarshalFromDatabase.
type pgAuction struct {
	ID                  uuid.UUID
	SellerID            uuid.UUID
	Title               string
	Description         string
	Currency            string
	StartPriceMinor     int64
	IncrementMinor      int64
	ReservePriceMinor   sql.NullInt64
	StartsAt            time.Time
	EndsAt              time.Time
	OriginalEndsAt      time.Time
	SnipeWindowSec      int64
	SnipeExtensionSec   int64
	SnipeMaxExt         int
	VerifyAboveMinor    int64
	ExtensionsUsed      int
	Status              string
	Outcome             sql.NullString
	LeadingBidID        uuid.NullUUID
	LeadingBidderID     uuid.NullUUID
	LeadingAmountMinor  sql.NullInt64
	LeadingPlacedAt     sql.NullTime
	RunnerUpBidID       uuid.NullUUID
	RunnerUpBidderID    uuid.NullUUID
	RunnerUpAmountMinor sql.NullInt64
	RunnerUpPlacedAt    sql.NullTime
	WinnerReassigned    bool
	BidCount            int
	RelistOf            uuid.NullUUID
	RelistGeneration    int
	Settled             bool
	Version             int64
}

const auctionColumns = `id, seller_id, title, description, currency,
	start_price_minor, increment_minor, reserve_price_minor,
	starts_at, ends_at, original_ends_at,
	snipe_window_sec, snipe_extension_sec, snipe_max_ext, verify_above_minor,
	extensions_used, status, outcome,
	leading_bid_id, leading_bidder_id, leading_amount_minor, leading_placed_at,
	runner_up_bid_id, runner_up_bidder_id, runner_up_amount_minor, runner_up_placed_at,
	winner_reassigned, bid_count, relist_of, relist_generation, settled, version`

func scanPgAuction(row *sql.Row) (pgAuction, error) {
	var p pgAuction
	err := row.Scan(
		&p.ID, &p.SellerID, &p.Title, &p.Description, &p.Currency,
		&p.StartPriceMinor, &p.IncrementMinor, &p.ReservePriceMinor,
		&p.StartsAt, &p.EndsAt, &p.OriginalEndsAt,
		&p.SnipeWindowSec, &p.SnipeExtensionSec, &p.SnipeMaxExt, &p.VerifyAboveMinor,
		&p.ExtensionsUsed, &p.Status, &p.Outcome,
		&p.LeadingBidID, &p.LeadingBidderID, &p.LeadingAmountMinor, &p.LeadingPlacedAt,
		&p.RunnerUpBidID, &p.RunnerUpBidderID, &p.RunnerUpAmountMinor, &p.RunnerUpPlacedAt,
		&p.WinnerReassigned, &p.BidCount, &p.RelistOf, &p.RelistGeneration, &p.Settled, &p.Version,
	)
	return p, err
}

func (r *AuctionPostgresRepository) Add(ctx context.Context, a *auction.Auction) error {
	if a == nil {
		return errors.New("auction must not be nil")
	}
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		p := toPgAuction(a)
		res, err := tx.ExecContext(ctx, `
			INSERT INTO auction.auctions (`+auctionColumns+`, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			        $19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32, now(), now())
			ON CONFLICT (id) DO NOTHING`,
			p.ID, p.SellerID, p.Title, p.Description, p.Currency,
			p.StartPriceMinor, p.IncrementMinor, p.ReservePriceMinor,
			p.StartsAt, p.EndsAt, p.OriginalEndsAt,
			p.SnipeWindowSec, p.SnipeExtensionSec, p.SnipeMaxExt, p.VerifyAboveMinor,
			p.ExtensionsUsed, p.Status, p.Outcome,
			p.LeadingBidID, p.LeadingBidderID, p.LeadingAmountMinor, p.LeadingPlacedAt,
			p.RunnerUpBidID, p.RunnerUpBidderID, p.RunnerUpAmountMinor, p.RunnerUpPlacedAt,
			p.WinnerReassigned, p.BidCount, p.RelistOf, p.RelistGeneration, p.Settled, p.Version,
		)
		if err != nil {
			if isUniqueViolation(err, "auctions_relist_of_uq") {
				return auction.ErrAlreadyRelisted
			}
			return fmt.Errorf("unable to insert auction into db: %w", err)
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("unable to read rows affected: %w", err)
		}
		if inserted == 0 {
			// Same client-generated id retried: silent no-op, no
			// events re-published.
			return nil
		}
		return r.publishIntegrationEvents(ctx, tx, a)
	})
}

func (r *AuctionPostgresRepository) Get(ctx context.Context, id auction.AuctionID) (*auction.Auction, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+auctionColumns+` FROM auction.auctions WHERE id = $1`, id.UUID())
	p, err := scanPgAuction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auction.NotFoundError{AuctionID: id}
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get auction from db: %w", err)
	}
	return toDomainAuction(p)
}

func (r *AuctionPostgresRepository) Update(
	ctx context.Context,
	id auction.AuctionID,
	actor auction.Actor,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	if actor.IsZero() {
		return errors.New("update requires an acting user; system flows use UpdateAsSystem")
	}
	return r.update(ctx, id, updateFn)
}

func (r *AuctionPostgresRepository) UpdateAsSystem(
	ctx context.Context,
	id auction.AuctionID,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	return r.update(ctx, id, updateFn)
}

func (r *AuctionPostgresRepository) update(
	ctx context.Context,
	id auction.AuctionID,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+auctionColumns+` FROM auction.auctions WHERE id = $1 FOR UPDATE`, id.UUID())
		p, err := scanPgAuction(row)
		if errors.Is(err, sql.ErrNoRows) {
			return auction.NotFoundError{AuctionID: id}
		}
		if err != nil {
			return fmt.Errorf("unable to lock auction row: %w", err)
		}
		a, err := toDomainAuction(p)
		if err != nil {
			return err
		}

		updated, err := updateFn(ctx, a)
		if err != nil {
			return err
		}

		up := toPgAuction(updated)
		res, err := tx.ExecContext(ctx, `
			UPDATE auction.auctions SET
				ends_at = $2, extensions_used = $3, status = $4, outcome = $5,
				leading_bid_id = $6, leading_bidder_id = $7, leading_amount_minor = $8, leading_placed_at = $9,
				runner_up_bid_id = $10, runner_up_bidder_id = $11, runner_up_amount_minor = $12, runner_up_placed_at = $13,
				winner_reassigned = $14, bid_count = $15, settled = $16,
				version = $17 + 1, updated_at = now()
			WHERE id = $1 AND version = $17`,
			up.ID,
			up.EndsAt, up.ExtensionsUsed, up.Status, up.Outcome,
			up.LeadingBidID, up.LeadingBidderID, up.LeadingAmountMinor, up.LeadingPlacedAt,
			up.RunnerUpBidID, up.RunnerUpBidderID, up.RunnerUpAmountMinor, up.RunnerUpPlacedAt,
			up.WinnerReassigned, up.BidCount, up.Settled,
			p.Version,
		)
		if err != nil {
			return fmt.Errorf("unable to update auction in db: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("unable to read rows affected: %w", err)
		}
		if affected == 0 {
			// Belt-and-suspenders to FOR UPDATE: cannot happen unless
			// the row was mutated outside the lock.
			return fmt.Errorf("optimistic lock conflict on auction %s (version %d)", id, p.Version)
		}

		// The aggregate's history is append-only: accepted bids are
		// taken from the recorded BidPlaced events, never updated.
		events := updated.PullDomainEvents()
		for _, e := range events {
			placed, ok := e.(auction.BidPlaced)
			if !ok {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auction.auction_bids (bid_id, auction_id, bidder_id, amount_minor, currency, placed_at)
				VALUES ($1,$2,$3,$4,$5,$6)`,
				placed.Bid.ID().UUID(), updated.ID().UUID(), placed.Bid.Bidder().UUID(),
				placed.Bid.Amount().AmountMinor(), placed.Bid.Amount().Currency().String(),
				placed.Bid.PlacedAt(),
			); err != nil {
				return fmt.Errorf("unable to append bid to history: %w", err)
			}
		}
		return r.publishMapped(ctx, tx, updated, events)
	})
}

// DueForClosing is the closing worker's lock-free candidate scan (§10),
// served by the partial index auctions_due_idx.
func (r *AuctionPostgresRepository) DueForClosing(ctx context.Context, before time.Time, limit int) ([]auction.AuctionID, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM auction.auctions
		WHERE status = 'listed' AND ends_at <= $1
		ORDER BY ends_at
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("unable to scan due auctions: %w", err)
	}
	defer rows.Close()

	var ids []auction.AuctionID
	for rows.Next() {
		var raw uuid.UUID
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("unable to scan due auction id: %w", err)
		}
		id, err := auction.NewAuctionID(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid auction id in db: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unable to iterate due auctions: %w", err)
	}
	return ids, nil
}

// publishIntegrationEvents drains the aggregate and publishes the
// mapped events; used by Add where no bids can exist yet.
func (r *AuctionPostgresRepository) publishIntegrationEvents(ctx context.Context, tx *sql.Tx, a *auction.Auction) error {
	return r.publishMapped(ctx, tx, a, a.PullDomainEvents())
}

// publishMapped maps domain events to integration V1 payloads and
// publishes them into the outbox bound to the open transaction: the
// event INSERT commits or rolls back together with the business write
// (rule 35).
func (r *AuctionPostgresRepository) publishMapped(ctx context.Context, tx *sql.Tx, a *auction.Auction, events []auction.DomainEvent) error {
	if len(events) == 0 {
		return nil
	}
	publisher, err := cwatermill.NewTxPublisher(tx, r.wmLogger)
	if err != nil {
		return fmt.Errorf("unable to create tx publisher: %w", err)
	}
	bus, err := cwatermill.NewEventBus(publisher,
		func(string) string { return auctionevents.Topic }, r.wmLogger)
	if err != nil {
		return fmt.Errorf("unable to create event bus: %w", err)
	}
	for _, e := range events {
		integration := mapDomainEvent(a, e)
		if integration == nil {
			continue
		}
		if err := bus.Publish(ctx, integration); err != nil {
			return fmt.Errorf("unable to publish %T to outbox: %w", integration, err)
		}
	}
	return nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == constraint
}

// --- mapping ---------------------------------------------------------

func toPgAuction(a *auction.Auction) pgAuction {
	p := pgAuction{
		ID:                a.ID().UUID(),
		SellerID:          a.Seller().UUID(),
		Title:             a.Lot().Title(),
		Description:       a.Lot().Description(),
		Currency:          a.StartPrice().Currency().String(),
		StartPriceMinor:   a.StartPrice().AmountMinor(),
		IncrementMinor:    a.Increment().AmountMinor(),
		StartsAt:          a.Window().StartsAt(),
		EndsAt:            a.Window().EndsAt(),
		OriginalEndsAt:    a.Window().OriginalEndsAt(),
		SnipeWindowSec:    int64(a.AntiSnipe().Window().Seconds()),
		SnipeExtensionSec: int64(a.AntiSnipe().Extension().Seconds()),
		SnipeMaxExt:       a.AntiSnipe().MaxExtensions(),
		VerifyAboveMinor:  a.VerifyAbove().AmountMinor(),
		ExtensionsUsed:    a.ExtensionsUsed(),
		Status:            a.Status().String(),
		WinnerReassigned:  a.WinnerReassigned(),
		BidCount:          a.BidCount(),
		RelistGeneration:  a.RelistGeneration(),
		Settled:           a.Settled(),
		Version:           a.Version(),
	}
	if !a.Reserve().IsZero() {
		p.ReservePriceMinor = sql.NullInt64{Int64: a.Reserve().Money().AmountMinor(), Valid: true}
	}
	if !a.Outcome().IsZero() {
		p.Outcome = sql.NullString{String: a.Outcome().String(), Valid: true}
	}
	if leading := a.LeadingBid(); !leading.IsZero() {
		p.LeadingBidID = uuid.NullUUID{UUID: leading.ID().UUID(), Valid: true}
		p.LeadingBidderID = uuid.NullUUID{UUID: leading.Bidder().UUID(), Valid: true}
		p.LeadingAmountMinor = sql.NullInt64{Int64: leading.Amount().AmountMinor(), Valid: true}
		p.LeadingPlacedAt = sql.NullTime{Time: leading.PlacedAt(), Valid: true}
	}
	if runnerUp := a.RunnerUpBid(); !runnerUp.IsZero() {
		p.RunnerUpBidID = uuid.NullUUID{UUID: runnerUp.ID().UUID(), Valid: true}
		p.RunnerUpBidderID = uuid.NullUUID{UUID: runnerUp.Bidder().UUID(), Valid: true}
		p.RunnerUpAmountMinor = sql.NullInt64{Int64: runnerUp.Amount().AmountMinor(), Valid: true}
		p.RunnerUpPlacedAt = sql.NullTime{Time: runnerUp.PlacedAt(), Valid: true}
	}
	if !a.RelistOf().IsZero() {
		p.RelistOf = uuid.NullUUID{UUID: a.RelistOf().UUID(), Valid: true}
	}
	return p
}

func toDomainAuction(p pgAuction) (*auction.Auction, error) {
	id, err := auction.NewAuctionID(p.ID)
	if err != nil {
		return nil, fmt.Errorf("invalid auction id in db: %w", err)
	}
	seller, err := auction.NewSellerID(p.SellerID)
	if err != nil {
		return nil, fmt.Errorf("invalid seller id in db: %w", err)
	}
	lot, err := auction.NewLot(p.Title, p.Description)
	if err != nil {
		return nil, fmt.Errorf("invalid lot in db: %w", err)
	}
	currency, err := auction.NewCurrency(p.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid currency in db: %w", err)
	}
	startPrice, err := auction.NewMoney(p.StartPriceMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid start price in db: %w", err)
	}
	increment, err := auction.NewMoney(p.IncrementMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid increment in db: %w", err)
	}
	reserve := auction.NoReserve()
	if p.ReservePriceMinor.Valid {
		reserveMoney, err := auction.NewMoney(p.ReservePriceMinor.Int64, currency)
		if err != nil {
			return nil, fmt.Errorf("invalid reserve in db: %w", err)
		}
		if reserve, err = auction.NewReservePrice(reserveMoney); err != nil {
			return nil, fmt.Errorf("invalid reserve in db: %w", err)
		}
	}
	window, err := auction.UnmarshalBiddingWindow(p.StartsAt, p.EndsAt, p.OriginalEndsAt)
	if err != nil {
		return nil, fmt.Errorf("invalid bidding window in db: %w", err)
	}
	antiSnipe, err := auction.NewAntiSnipePolicy(
		time.Duration(p.SnipeWindowSec)*time.Second,
		time.Duration(p.SnipeExtensionSec)*time.Second,
		p.SnipeMaxExt,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid anti-snipe policy in db: %w", err)
	}
	verifyAbove, err := auction.NewMoney(p.VerifyAboveMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid verify threshold in db: %w", err)
	}
	status, err := auction.NewStatusFromString(p.Status)
	if err != nil {
		return nil, fmt.Errorf("invalid status in db: %w", err)
	}
	var outcome auction.Outcome
	if p.Outcome.Valid {
		if outcome, err = auction.NewOutcomeFromString(p.Outcome.String); err != nil {
			return nil, fmt.Errorf("invalid outcome in db: %w", err)
		}
	}
	leadingBid, err := bidFromColumns(p.LeadingBidID, p.LeadingBidderID, p.LeadingAmountMinor, p.LeadingPlacedAt, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid leading bid in db: %w", err)
	}
	runnerUpBid, err := bidFromColumns(p.RunnerUpBidID, p.RunnerUpBidderID, p.RunnerUpAmountMinor, p.RunnerUpPlacedAt, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid runner-up bid in db: %w", err)
	}
	var relistOf auction.AuctionID
	if p.RelistOf.Valid {
		if relistOf, err = auction.NewAuctionID(p.RelistOf.UUID); err != nil {
			return nil, fmt.Errorf("invalid relist_of in db: %w", err)
		}
	}

	return auction.UnmarshalFromDatabase(
		id, seller, lot,
		startPrice, increment, reserve,
		window, antiSnipe, verifyAbove,
		p.ExtensionsUsed, status, outcome,
		leadingBid, runnerUpBid,
		p.WinnerReassigned, p.BidCount,
		relistOf, p.RelistGeneration, p.Settled,
		p.Version,
	)
}

func bidFromColumns(bidID, bidderID uuid.NullUUID, amount sql.NullInt64, placedAt sql.NullTime, currency auction.Currency) (auction.Bid, error) {
	if !bidID.Valid {
		return auction.Bid{}, nil
	}
	if !bidderID.Valid || !amount.Valid || !placedAt.Valid {
		return auction.Bid{}, errors.New("partially populated bid columns")
	}
	typedBidID, err := auction.NewBidID(bidID.UUID)
	if err != nil {
		return auction.Bid{}, err
	}
	typedBidderID, err := auction.NewBidderID(bidderID.UUID)
	if err != nil {
		return auction.Bid{}, err
	}
	money, err := auction.NewMoney(amount.Int64, currency)
	if err != nil {
		return auction.Bid{}, err
	}
	return auction.NewBid(typedBidID, typedBidderID, money, placedAt.Time)
}
