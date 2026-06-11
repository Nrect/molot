//go:build integration

package adapters_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"molot/internal/auction/adapters"
	"molot/internal/auction/domain/auction"
	auctionevents "molot/internal/auction/events"
	"molot/internal/common/errs"
	"molot/internal/common/logs"
	"molot/internal/common/postgres"
	cwatermill "molot/internal/common/watermill"
)

// Integration level: real Postgres from docker compose. Gated by the
// `integration` build tag and TEST_DATABASE_URL; migrations run once
// per process through a goose provider with the context's own version
// table (goose_db_version_auction).

var (
	pgOnce sync.Once
	pgDB   *sql.DB
	pgErr  error
)

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping postgres integration tests")
	}
	pgOnce.Do(func() {
		ctx := context.Background()
		pgDB, pgErr = postgres.NewDB(ctx, dsn)
		if pgErr != nil {
			return
		}
		store, err := database.NewStore(database.DialectPostgres, "goose_db_version_auction")
		if err != nil {
			pgErr = err
			return
		}
		provider, err := goose.NewProvider("", pgDB, adapters.MigrationsForTests(), goose.WithStore(store))
		if err != nil {
			pgErr = err
			return
		}
		if _, err := provider.Up(ctx); err != nil {
			pgErr = err
			return
		}
		// Initialize the outbox topic schema (in production the
		// service constructor does it).
		wmLogger := cwatermill.NewLogger(logs.NewLogger("text"))
		subscriber, err := cwatermill.NewSQLSubscriber(pgDB, "adapters-test-init", 100*time.Millisecond, wmLogger)
		if err != nil {
			pgErr = err
			return
		}
		defer subscriber.Close()
		initializer, ok := subscriber.(interface{ SubscribeInitialize(string) error })
		if !ok {
			pgErr = errors.New("subscriber cannot initialize schema")
			return
		}
		pgErr = initializer.SubscribeInitialize(auctionevents.Topic)
	})
	if pgErr != nil {
		t.Fatalf("postgres test setup: %v", pgErr)
	}
	return pgDB
}

func pgRepo(t *testing.T) *adapters.AuctionPostgresRepository {
	t.Helper()
	db := pgTestDB(t)
	wmLogger := cwatermill.NewLogger(logs.NewLogger("text"))
	return adapters.NewAuctionPostgresRepository(db, wmLogger)
}

// TestAuctionPostgresRepository runs the same shared suite as the
// in-memory adapter (rule 42) — including both race tests, now against
// real row locks.
func TestAuctionPostgresRepository(t *testing.T) {
	t.Parallel()
	runRepositorySuite(t, func(t *testing.T) testRepository {
		return pgRepo(t)
	})
}

func outboxCountFor(t *testing.T, db *sql.DB, auctionID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "watermill_auction-events" WHERE payload->>'auction_id' = $1 OR payload->>'original_auction_id' = $1`,
		auctionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestPostgresOutbox: integration events commit atomically with the
// aggregate write — present after success, absent after rollback.
func TestPostgresOutbox(t *testing.T) {
	t.Parallel()
	repo := pgRepo(t)
	db := pgTestDB(t)
	ctx := context.Background()

	a := listedAuctionFx(t)
	if err := repo.Add(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := outboxCountFor(t, db, a.ID().String()); got != 1 {
		t.Fatalf("outbox rows after Add = %d, want 1 (AuctionListedV1)", got)
	}

	// Retried Add: no duplicate events.
	if err := repo.Add(ctx, rebuildWithID(t, a, a.ID())); err != nil {
		t.Fatal(err)
	}
	if got := outboxCountFor(t, db, a.ID().String()); got != 1 {
		t.Fatalf("outbox rows after retried Add = %d, want still 1", got)
	}

	// Successful bid publishes BidPlacedV1.
	bidder := verifiedBidderFx(t)
	err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
		func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
			if _, err := current.PlaceBid(newBidIDFx(t), bidder, eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
				return nil, err
			}
			return current, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := outboxCountFor(t, db, a.ID().String()); got != 2 {
		t.Fatalf("outbox rows after bid = %d, want 2", got)
	}

	// Failed update: rollback removes the event with the state change.
	sabotage := errors.New("sabotage")
	err = repo.UpdateAsSystem(ctx, a.ID(),
		func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
			if _, err := current.Close(t0.Add(25 * time.Hour)); err != nil {
				return nil, err
			}
			return nil, sabotage
		})
	if !errors.Is(err, sabotage) {
		t.Fatal(err)
	}
	if got := outboxCountFor(t, db, a.ID().String()); got != 2 {
		t.Fatalf("outbox rows after rollback = %d, want still 2", got)
	}
}

// TestPostgresBidHistoryAppend: accepted bids land in the append-only
// auction_bids log in the same transaction.
func TestPostgresBidHistoryAppend(t *testing.T) {
	t.Parallel()
	repo := pgRepo(t)
	db := pgTestDB(t)
	ctx := context.Background()

	a := listedAuctionFx(t)
	if err := repo.Add(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i, amount := range []int64{1000, 1100, 1200} {
		bidder := verifiedBidderFx(t)
		err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				if _, err := current.PlaceBid(newBidIDFx(t), bidder, eurFx(t, amount), t0.Add(time.Duration(i+1)*time.Hour)); err != nil {
					return nil, err
				}
				return current, nil
			})
		if err != nil {
			t.Fatal(err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM auction.auction_bids WHERE auction_id = $1`, a.ID().UUID()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("auction_bids rows = %d, want 3 (append-only, all bids kept)", count)
	}
}

// TestPostgresProjectionsAndReadModels covers the projection adapters'
// idempotency (each operation applied twice) and the read models on
// top of them.
func TestPostgresProjectionsAndReadModels(t *testing.T) {
	t.Parallel()
	db := pgTestDB(t)
	repo := pgRepo(t)
	ctx := context.Background()

	catalog := adapters.NewCatalogPostgresProjection(db)
	dashboard := adapters.NewDashboardPostgresProjection(db)
	profiles := adapters.NewBidderProfilesPostgres(db)
	readModels := adapters.NewAuctionPostgresReadModels(db)

	// The catalog ApplyBid joins increment from the write table — the
	// aggregate row must exist.
	a := listedAuctionFx(t)
	if err := repo.Add(ctx, a); err != nil {
		t.Fatal(err)
	}
	auctionID := a.ID().UUID()
	sellerID := a.Seller().UUID()
	endsAt := a.EndsAt()

	// Listed twice → one row (idempotent).
	for i := 0; i < 2; i++ {
		if err := catalog.UpsertListed(ctx, auctionID, "Bronze hammer", sellerID, "EUR", 1000, endsAt); err != nil {
			t.Fatal(err)
		}
		if err := dashboard.UpsertListed(ctx, auctionID, sellerID, "Bronze hammer", "EUR", endsAt); err != nil {
			t.Fatal(err)
		}
	}

	// Bid (count 1) twice → applied once (monotonic guard).
	for i := 0; i < 2; i++ {
		if err := catalog.ApplyBid(ctx, auctionID, 1500, 1, endsAt); err != nil {
			t.Fatal(err)
		}
		if err := dashboard.ApplyBid(ctx, auctionID, 1, endsAt); err != nil {
			t.Fatal(err)
		}
	}
	// Stale bid (count 0 again) → no-op, not an error.
	if err := catalog.ApplyBid(ctx, auctionID, 999, 1, endsAt); err != nil {
		t.Fatal(err)
	}

	page, err := readModels.ActiveAuctions(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var item *struct {
		current, minNext int64
		bidCount         int
	}
	for _, it := range page.Items {
		if it.AuctionID == a.ID().String() {
			item = &struct {
				current, minNext int64
				bidCount         int
			}{it.CurrentPriceMinor, it.MinimalNextBidMinor, it.BidCount}
		}
	}
	if item == nil {
		t.Fatal("catalog item missing")
	}
	if item.current != 1500 || item.minNext != 1600 || item.bidCount != 1 {
		t.Fatalf("catalog item = %+v, want price 1500, min next 1600 (price+increment), 1 bid", *item)
	}

	// Profiles: registered → unverified; verified flips; redelivery of
	// registered must NOT reset verification.
	bidderID := uuid.New()
	if err := profiles.UpsertRegistered(ctx, bidderID, "Alyosha"); err != nil {
		t.Fatal(err)
	}
	if err := profiles.MarkVerified(ctx, bidderID); err != nil {
		t.Fatal(err)
	}
	if err := profiles.UpsertRegistered(ctx, bidderID, "Alyosha"); err != nil {
		t.Fatal(err)
	}
	typedBidder, err := auction.NewBidderID(bidderID)
	if err != nil {
		t.Fatal(err)
	}
	bidder, err := profiles.BidderByID(ctx, typedBidder)
	if err != nil {
		t.Fatal(err)
	}
	if !bidder.Verified() {
		t.Fatal("redelivered registration must not undo verification")
	}
	// Unknown bidder = unverified (documented contract).
	unknown, err := auction.NewBidderID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	b, err := profiles.BidderByID(ctx, unknown)
	if err != nil {
		t.Fatal(err)
	}
	if b.Verified() {
		t.Fatal("unknown bidder must be unverified")
	}

	// Close: catalog row removed (twice — idempotent), dashboard updated.
	for i := 0; i < 2; i++ {
		if err := catalog.Remove(ctx, auctionID); err != nil {
			t.Fatal(err)
		}
		if err := dashboard.ApplyClosed(ctx, auctionID, "sold", 1500); err != nil {
			t.Fatal(err)
		}
		if err := dashboard.ApplySettlement(ctx, auctionID, "settled", ""); err != nil {
			t.Fatal(err)
		}
	}

	typedSeller, err := auction.NewSellerID(sellerID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := readModels.Dashboard(ctx, typedSeller)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 {
		t.Fatalf("dashboard items = %d, want 1", len(view.Items))
	}
	got := view.Items[0]
	if got.Status != "closed" || got.Outcome != "sold" || got.SettlementStatus != "settled" || got.HammerPriceMinor != 1500 {
		t.Fatalf("dashboard item = %+v", got)
	}
	if view.SoldTotalMinor != 1500 || view.ActiveCount != 0 {
		t.Fatalf("dashboard aggregates = %+v", view)
	}

	// Projection lag: a bid for a never-projected, still-listed auction
	// must be retryable (error), not silently dropped.
	lagging := listedAuctionFx(t)
	if err := repo.Add(ctx, lagging); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplyBid(ctx, lagging.ID().UUID(), 1000, 1, lagging.EndsAt()); err == nil {
		t.Fatal("bid against unprojected listed auction must error for retry")
	}
}

// TestPostgresAuctionCardReadModel reads the card directly from the
// write tables.
func TestPostgresAuctionCardReadModel(t *testing.T) {
	t.Parallel()
	db := pgTestDB(t)
	repo := pgRepo(t)
	readModels := adapters.NewAuctionPostgresReadModels(db)
	ctx := context.Background()

	a := listedAuctionFx(t, withReserveFx(t, 5000))
	if err := repo.Add(ctx, a); err != nil {
		t.Fatal(err)
	}
	bidder := verifiedBidderFx(t)
	err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
		func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
			if _, err := current.PlaceBid(newBidIDFx(t), bidder, eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
				return nil, err
			}
			return current, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	card, err := readModels.AuctionCard(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !card.HasReserve {
		t.Fatal("card must flag the reserve")
	}
	if card.CurrentPriceMinor != 1000 || card.MinimalNextBidMinor != 1100 {
		t.Fatalf("card prices = %d/%d", card.CurrentPriceMinor, card.MinimalNextBidMinor)
	}
	if card.LeaderID != bidder.ID().String() {
		t.Fatal("card leader mismatch")
	}
	if len(card.RecentBids) != 1 {
		t.Fatalf("recent bids = %d, want 1", len(card.RecentBids))
	}

	history, err := readModels.Bids(ctx, a.ID(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || len(history.Items) != 1 {
		t.Fatalf("history = %+v", history)
	}

	// Missing card → 404 slug.
	_, err = readModels.AuctionCard(ctx, newAuctionIDFx(t))
	if !errors.Is(err, errs.NewNotFoundError("auction-not-found")) {
		t.Fatalf("err = %v, want auction-not-found", err)
	}
}
