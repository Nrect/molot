// Package adapters holds the settlement context's infrastructure: the
// Postgres and in-memory repositories, the gateway adapters to the
// auction/billing facades and the embedded migrations.
package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"molot/internal/common/postgres"
	"molot/internal/settlement/app/query"
	"molot/internal/settlement/domain/settlement"
)

const settlementColumns = `auction_id, state, winner_id, hammer_minor, currency,
	runner_up_id, runner_up_minor, runner_up_qualifies, relist_generation,
	attempt, invoice_id, failure_reason, version`

// SettlementPostgresRepository is the production settlement.Repository
// and, on the read side, the query.SettlementReadModel (direct reads of
// the write table — "not every query needs a read model", §5). It is
// dumb on purpose: load → map → guard → persist; zero business rules.
//
// Observability (§11): every committed state transition increments
// molot_saga_transitions_total{from,to,reason} — the saga flow panel.
type SettlementPostgresRepository struct {
	db          *sql.DB
	transitions metric.Int64Counter
}

func NewSettlementPostgresRepository(db *sql.DB, meterProvider metric.MeterProvider) *SettlementPostgresRepository {
	if db == nil {
		panic("NewSettlementPostgresRepository: nil db")
	}
	if meterProvider == nil {
		panic("NewSettlementPostgresRepository: nil meter provider")
	}
	meter := meterProvider.Meter("molot/internal/settlement/adapters")
	transitions, err := meter.Int64Counter("molot_saga_transitions_total",
		metric.WithDescription("Committed settlement saga state transitions"))
	if err != nil {
		panic("NewSettlementPostgresRepository: create transitions counter: " + err.Error())
	}
	return &SettlementPostgresRepository{db: db, transitions: transitions}
}

// pgSettlement is the storage transport struct (rule 7): the domain
// aggregate is rebuilt only through settlement.UnmarshalFromDatabase.
type pgSettlement struct {
	AuctionID         uuid.UUID
	State             string
	WinnerID          uuid.UUID
	HammerMinor       int64
	Currency          string
	RunnerUpID        uuid.NullUUID
	RunnerUpMinor     sql.NullInt64
	RunnerUpQualifies bool
	RelistGeneration  int
	Attempt           int
	InvoiceID         uuid.NullUUID
	FailureReason     sql.NullString
	Version           int64
}

// Add inserts the freshly started saga; ON CONFLICT (auction_id) DO
// NOTHING makes the redelivered AuctionClosedV1 a silent no-op (§6.1).
func (r *SettlementPostgresRepository) Add(ctx context.Context, s *settlement.Settlement) error {
	if s == nil {
		return errors.New("settlement must not be nil")
	}
	p := toPgSettlement(s)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO settlement.settlements
			(`+settlementColumns+`, started_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now(), now())
		ON CONFLICT (auction_id) DO NOTHING`,
		p.AuctionID, p.State, p.WinnerID, p.HammerMinor, p.Currency,
		p.RunnerUpID, p.RunnerUpMinor, p.RunnerUpQualifies, p.RelistGeneration,
		p.Attempt, p.InvoiceID, p.FailureReason, p.Version,
	)
	if err != nil {
		return fmt.Errorf("unable to insert settlement: %w", err)
	}
	return nil
}

func (r *SettlementPostgresRepository) Get(ctx context.Context, id settlement.AuctionID) (*settlement.Settlement, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+settlementColumns+` FROM settlement.settlements WHERE auction_id = $1`,
		id.UUID(),
	)
	s, err := scanSettlement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, settlement.NotFoundError{AuctionID: id}
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get settlement from db: %w", err)
	}
	return s, nil
}

// Update is the saga's commit phase (§6.1): SELECT ... FOR UPDATE,
// updateFn, persist with the version check on top — and the
// transitions counter once the transaction is committed.
func (r *SettlementPostgresRepository) Update(
	ctx context.Context,
	id settlement.AuctionID,
	updateFn func(ctx context.Context, s *settlement.Settlement) (*settlement.Settlement, error),
) error {
	var from, to, reason string

	err := postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+settlementColumns+` FROM settlement.settlements WHERE auction_id = $1 FOR UPDATE`,
			id.UUID(),
		)
		s, err := scanSettlement(row)
		if errors.Is(err, sql.ErrNoRows) {
			return settlement.NotFoundError{AuctionID: id}
		}
		if err != nil {
			return fmt.Errorf("unable to get settlement from db: %w", err)
		}

		expectedVersion := s.Version()
		from = s.State().String()

		updated, err := updateFn(ctx, s)
		if err != nil {
			return err
		}
		to = updated.State().String()
		reason = updated.FailureReason().String()

		p := toPgSettlement(updated)
		res, err := tx.ExecContext(ctx, `
			UPDATE settlement.settlements
			SET state = $2, winner_id = $3, attempt = $4, invoice_id = $5,
			    failure_reason = $6, version = version + 1, updated_at = now()
			WHERE auction_id = $1 AND version = $7`,
			p.AuctionID, p.State, p.WinnerID, p.Attempt, p.InvoiceID, p.FailureReason, expectedVersion,
		)
		if err != nil {
			return fmt.Errorf("unable to update settlement: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("unable to read update result: %w", err)
		}
		if affected != 1 {
			// Belt-and-suspenders on top of FOR UPDATE.
			return fmt.Errorf("concurrent modification of settlement %s (version %d)", id, expectedVersion)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if from != to {
		r.transitions.Add(ctx, 1, metric.WithAttributes(
			attribute.String("from", from),
			attribute.String("to", to),
			attribute.String("reason", reason),
		))
	}
	return nil
}

// --- read side (query.SettlementReadModel) ----------------------------------

func (r *SettlementPostgresRepository) SettlementByAuction(
	ctx context.Context,
	id settlement.AuctionID,
) (query.SettlementView, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+settlementColumns+` FROM settlement.settlements WHERE auction_id = $1`,
		id.UUID(),
	)
	var t pgSettlement
	if err := scanSettlementRow(row, &t); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return query.SettlementView{}, settlement.NotFoundError{AuctionID: id}
		}
		return query.SettlementView{}, fmt.Errorf("unable to read settlement view: %w", err)
	}
	return toSettlementView(t), nil
}

func toSettlementView(t pgSettlement) query.SettlementView {
	v := query.SettlementView{
		AuctionID:         t.AuctionID.String(),
		State:             t.State,
		WinnerID:          t.WinnerID.String(),
		HammerMinor:       t.HammerMinor,
		Currency:          t.Currency,
		RunnerUpQualifies: t.RunnerUpQualifies,
		RelistGeneration:  t.RelistGeneration,
		Attempt:           t.Attempt,
	}
	if t.RunnerUpID.Valid {
		v.RunnerUpID = t.RunnerUpID.UUID.String()
		v.RunnerUpMinor = t.RunnerUpMinor.Int64
	}
	if t.InvoiceID.Valid {
		v.InvoiceID = t.InvoiceID.UUID.String()
	}
	if t.FailureReason.Valid {
		v.FailureReason = t.FailureReason.String
	}
	return v
}

// --- scanning and mapping -----------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSettlementRow(row rowScanner, t *pgSettlement) error {
	return row.Scan(
		&t.AuctionID, &t.State, &t.WinnerID, &t.HammerMinor, &t.Currency,
		&t.RunnerUpID, &t.RunnerUpMinor, &t.RunnerUpQualifies, &t.RelistGeneration,
		&t.Attempt, &t.InvoiceID, &t.FailureReason, &t.Version,
	)
}

func scanSettlement(row rowScanner) (*settlement.Settlement, error) {
	var t pgSettlement
	if err := scanSettlementRow(row, &t); err != nil {
		return nil, err
	}
	return toDomainSettlement(t)
}

func toDomainSettlement(t pgSettlement) (*settlement.Settlement, error) {
	auctionID, err := settlement.NewAuctionID(t.AuctionID)
	if err != nil {
		return nil, fmt.Errorf("invalid auction id in db: %w", err)
	}
	state, err := settlement.NewStateFromString(t.State)
	if err != nil {
		return nil, fmt.Errorf("invalid state in db: %w", err)
	}
	winner, err := settlement.NewBidderID(t.WinnerID)
	if err != nil {
		return nil, fmt.Errorf("invalid winner id in db: %w", err)
	}
	currency, err := settlement.NewCurrency(t.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid currency in db: %w", err)
	}
	hammer, err := settlement.NewMoney(t.HammerMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid hammer amount in db: %w", err)
	}
	var runnerUp settlement.BidderID
	var runnerUpAmount settlement.Money
	if t.RunnerUpID.Valid {
		if runnerUp, err = settlement.NewBidderID(t.RunnerUpID.UUID); err != nil {
			return nil, fmt.Errorf("invalid runner-up id in db: %w", err)
		}
		if runnerUpAmount, err = settlement.NewMoney(t.RunnerUpMinor.Int64, currency); err != nil {
			return nil, fmt.Errorf("invalid runner-up amount in db: %w", err)
		}
	}
	var invoiceID settlement.InvoiceID
	if t.InvoiceID.Valid {
		if invoiceID, err = settlement.NewInvoiceID(t.InvoiceID.UUID); err != nil {
			return nil, fmt.Errorf("invalid invoice id in db: %w", err)
		}
	}
	var failureReason settlement.FailureReason
	if t.FailureReason.Valid {
		if failureReason, err = settlement.NewFailureReasonFromString(t.FailureReason.String); err != nil {
			return nil, fmt.Errorf("invalid failure reason in db: %w", err)
		}
	}
	return settlement.UnmarshalFromDatabase(
		auctionID, state, winner, hammer,
		runnerUp, runnerUpAmount, t.RunnerUpQualifies,
		t.RelistGeneration, t.Attempt, invoiceID, failureReason, t.Version,
	)
}

func toPgSettlement(s *settlement.Settlement) pgSettlement {
	p := pgSettlement{
		AuctionID:         s.AuctionID().UUID(),
		State:             s.State().String(),
		WinnerID:          s.Winner().UUID(),
		HammerMinor:       s.Hammer().Amount(),
		Currency:          s.Hammer().Currency().String(),
		RunnerUpQualifies: s.RunnerUpQualifies(),
		RelistGeneration:  s.RelistGeneration(),
		Attempt:           s.Attempt(),
		Version:           s.Version(),
	}
	if !s.RunnerUp().IsZero() {
		p.RunnerUpID = uuid.NullUUID{UUID: s.RunnerUp().UUID(), Valid: true}
		p.RunnerUpMinor = sql.NullInt64{Int64: s.RunnerUpAmount().Amount(), Valid: true}
	}
	if !s.InvoiceID().IsZero() {
		p.InvoiceID = uuid.NullUUID{UUID: s.InvoiceID().UUID(), Valid: true}
	}
	if !s.FailureReason().IsZero() {
		p.FailureReason = sql.NullString{String: s.FailureReason().String(), Valid: true}
	}
	return p
}
