package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	wm "github.com/ThreeDotsLabs/watermill"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"molot/internal/billing/app/query"
	"molot/internal/billing/domain/invoice"
	"molot/internal/common/postgres"
)

const pgUniqueViolation = "23505"

const invoiceColumns = `id, auction_id, debtor_id, hammer_minor, commission_minor,
	total_minor, currency, status, due_at, attempt, psp_ref, version`

// InvoicePostgresRepository is the production invoice.Repository and,
// on the read side, the query.InvoiceReadModel (direct reads of the
// write table — "not every query needs a read model", §5). It is dumb
// on purpose: load → map → guard → persist; zero business rules.
type InvoicePostgresRepository struct {
	db       *sql.DB
	logger   *slog.Logger
	wmLogger wm.LoggerAdapter
}

func NewInvoicePostgresRepository(db *sql.DB, logger *slog.Logger, wmLogger wm.LoggerAdapter) *InvoicePostgresRepository {
	if db == nil {
		panic("NewInvoicePostgresRepository: nil db")
	}
	if logger == nil {
		panic("NewInvoicePostgresRepository: nil logger")
	}
	if wmLogger == nil {
		panic("NewInvoicePostgresRepository: nil watermill logger")
	}
	return &InvoicePostgresRepository{db: db, logger: logger, wmLogger: wmLogger}
}

// pgInvoice is the storage transport struct (BOOK_AUDIT rule 7): the
// domain aggregate is rebuilt only through invoice.UnmarshalFromDatabase.
type pgInvoice struct {
	ID              uuid.UUID
	AuctionID       uuid.UUID
	DebtorID        uuid.UUID
	HammerMinor     int64
	CommissionMinor int64
	TotalMinor      int64
	Currency        string
	Status          string
	DueAt           time.Time
	Attempt         int
	PSPRef          sql.NullString
	Version         int64
}

func (r *InvoicePostgresRepository) Add(ctx context.Context, inv *invoice.Invoice) error {
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO billing.invoices
				(id, auction_id, debtor_id, hammer_minor, commission_minor,
				 total_minor, currency, status, due_at, attempt, psp_ref,
				 version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), now())`,
			inv.ID().UUID(), inv.AuctionID().UUID(), inv.Debtor().UUID(),
			inv.Hammer().Amount(), inv.Commission().Amount(), inv.Total().Amount(),
			inv.Total().Currency().String(), inv.Status().String(), inv.DueAt(),
			inv.Attempt().Int(), nullablePSPRef(inv.PSPRef()), inv.Version(),
		)
		if isUniqueViolation(err) {
			// Deterministic id or UNIQUE(auction_id, attempt): the
			// invoice already exists — IssueInvoice maps this to nil.
			return invoice.ErrInvoiceAlreadyIssued
		}
		if err != nil {
			return fmt.Errorf("unable to insert invoice: %w", err)
		}
		return publishDomainEvents(ctx, tx, r.wmLogger, inv.PullDomainEvents())
	})
}

func (r *InvoicePostgresRepository) Get(ctx context.Context, id invoice.InvoiceID, actor invoice.BidderID) (*invoice.Invoice, error) {
	// Ownership lives in the WHERE predicate: a foreign invoice is the
	// same NotFoundError as a missing one (anti-enumeration, §2.2).
	row := r.db.QueryRowContext(ctx,
		`SELECT `+invoiceColumns+` FROM billing.invoices WHERE id = $1 AND debtor_id = $2`,
		id.UUID(), actor.UUID(),
	)
	inv, err := scanInvoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invoice.NotFoundError{InvoiceID: id}
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get invoice from db: %w", err)
	}
	return inv, nil
}

func (r *InvoicePostgresRepository) Update(
	ctx context.Context,
	id invoice.InvoiceID,
	actor invoice.BidderID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	return r.update(ctx, id, &actor, updateFn)
}

func (r *InvoicePostgresRepository) UpdateAsSystem(
	ctx context.Context,
	id invoice.InvoiceID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	return r.update(ctx, id, nil, updateFn)
}

func (r *InvoicePostgresRepository) update(
	ctx context.Context,
	id invoice.InvoiceID,
	actor *invoice.BidderID,
	updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+invoiceColumns+` FROM billing.invoices WHERE id = $1 FOR UPDATE`,
			id.UUID(),
		)
		inv, err := scanInvoice(row)
		if errors.Is(err, sql.ErrNoRows) {
			return invoice.NotFoundError{InvoiceID: id}
		}
		if err != nil {
			return fmt.Errorf("unable to get invoice from db: %w", err)
		}

		if actor != nil {
			// Authorization is a pure domain rule executed inside the
			// transaction (BOOK_AUDIT rule 22). Its violation is logged
			// at WARN for the audit trail and leaves the repository as
			// NotFoundError — a foreign invoice must be
			// indistinguishable from a missing one (§2.2).
			if accessErr := invoice.CanDebtorAccessInvoice(*actor, *inv); accessErr != nil {
				r.logger.WarnContext(ctx, "forbidden invoice access",
					slog.String("context", "billing"),
					slog.String("invoice_id", id.String()),
					slog.Any("error", accessErr),
				)
				return invoice.NotFoundError{InvoiceID: id}
			}
		}

		expectedVersion := inv.Version()
		updated, err := updateFn(ctx, inv)
		if err != nil {
			return err
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE billing.invoices
			SET status = $2, psp_ref = $3, version = version + 1, updated_at = now()
			WHERE id = $1 AND version = $4`,
			id.UUID(), updated.Status().String(), nullablePSPRef(updated.PSPRef()), expectedVersion,
		)
		if err != nil {
			return fmt.Errorf("unable to update invoice: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("unable to read update result: %w", err)
		}
		if affected != 1 {
			// Belt-and-suspenders on top of FOR UPDATE.
			return fmt.Errorf("concurrent modification of invoice %s (version %d)", id, expectedVersion)
		}
		return publishDomainEvents(ctx, tx, r.wmLogger, updated.PullDomainEvents())
	})
}

func (r *InvoicePostgresRepository) PendingDueBefore(ctx context.Context, t time.Time, limit int) ([]invoice.InvoiceID, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM billing.invoices
		WHERE status = 'pending' AND due_at <= $1
		ORDER BY due_at
		LIMIT $2`, t, limit)
	if err != nil {
		return nil, fmt.Errorf("unable to scan due invoices: %w", err)
	}
	defer rows.Close()

	var ids []invoice.InvoiceID
	for rows.Next() {
		var raw uuid.UUID
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("unable to scan due invoice id: %w", err)
		}
		id, err := invoice.NewInvoiceID(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid invoice id in db: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unable to iterate due invoices: %w", err)
	}
	return ids, nil
}

// --- read side (query.InvoiceReadModel) ------------------------------------

func (r *InvoicePostgresRepository) InvoiceByID(ctx context.Context, id invoice.InvoiceID, actor invoice.BidderID) (query.InvoiceView, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+invoiceColumns+` FROM billing.invoices WHERE id = $1 AND debtor_id = $2`,
		id.UUID(), actor.UUID(),
	)
	var t pgInvoice
	if err := scanInvoiceRow(row, &t); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return query.InvoiceView{}, invoice.NotFoundError{InvoiceID: id}
		}
		return query.InvoiceView{}, fmt.Errorf("unable to read invoice view: %w", err)
	}
	return toInvoiceView(t), nil
}

func (r *InvoicePostgresRepository) PendingOfBidder(ctx context.Context, b invoice.BidderID) ([]query.InvoiceView, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+invoiceColumns+` FROM billing.invoices
		 WHERE debtor_id = $1 AND status = 'pending'
		 ORDER BY due_at`,
		b.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to read pending invoices: %w", err)
	}
	defer rows.Close()

	views := []query.InvoiceView{}
	for rows.Next() {
		var t pgInvoice
		if err := scanInvoiceRow(rows, &t); err != nil {
			return nil, fmt.Errorf("unable to scan pending invoice: %w", err)
		}
		views = append(views, toInvoiceView(t))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unable to iterate pending invoices: %w", err)
	}
	return views, nil
}

func toInvoiceView(t pgInvoice) query.InvoiceView {
	return query.InvoiceView{
		InvoiceID:       t.ID.String(),
		AuctionID:       t.AuctionID.String(),
		Status:          t.Status,
		HammerMinor:     t.HammerMinor,
		CommissionMinor: t.CommissionMinor,
		TotalMinor:      t.TotalMinor,
		Currency:        t.Currency,
		DueAt:           t.DueAt,
		Attempt:         t.Attempt,
	}
}

// --- scanning and mapping ---------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInvoiceRow(row rowScanner, t *pgInvoice) error {
	return row.Scan(
		&t.ID, &t.AuctionID, &t.DebtorID, &t.HammerMinor, &t.CommissionMinor,
		&t.TotalMinor, &t.Currency, &t.Status, &t.DueAt, &t.Attempt, &t.PSPRef, &t.Version,
	)
}

func scanInvoice(row rowScanner) (*invoice.Invoice, error) {
	var t pgInvoice
	if err := scanInvoiceRow(row, &t); err != nil {
		return nil, err
	}
	return toDomainInvoice(t)
}

func toDomainInvoice(t pgInvoice) (*invoice.Invoice, error) {
	id, err := invoice.NewInvoiceID(t.ID)
	if err != nil {
		return nil, fmt.Errorf("invalid invoice id in db: %w", err)
	}
	auctionID, err := invoice.NewAuctionID(t.AuctionID)
	if err != nil {
		return nil, fmt.Errorf("invalid auction id in db: %w", err)
	}
	debtor, err := invoice.NewBidderID(t.DebtorID)
	if err != nil {
		return nil, fmt.Errorf("invalid debtor id in db: %w", err)
	}
	currency, err := invoice.NewCurrency(t.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid currency in db: %w", err)
	}
	hammer, err := invoice.NewMoney(t.HammerMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid hammer amount in db: %w", err)
	}
	commission, err := invoice.NewMoney(t.CommissionMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid commission amount in db: %w", err)
	}
	total, err := invoice.NewMoney(t.TotalMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid total amount in db: %w", err)
	}
	status, err := invoice.NewInvoiceStatusFromString(t.Status)
	if err != nil {
		return nil, fmt.Errorf("invalid status in db: %w", err)
	}
	attempt, err := invoice.NewAttempt(t.Attempt)
	if err != nil {
		return nil, fmt.Errorf("invalid attempt in db: %w", err)
	}
	var pspRef invoice.PaymentReference
	if t.PSPRef.Valid {
		pspRef, err = invoice.NewPaymentReference(t.PSPRef.String)
		if err != nil {
			return nil, fmt.Errorf("invalid psp reference in db: %w", err)
		}
	}
	return invoice.UnmarshalFromDatabase(
		id, auctionID, debtor, hammer, commission, total,
		status, t.DueAt, attempt, pspRef, t.Version,
	)
}

func nullablePSPRef(ref invoice.PaymentReference) sql.NullString {
	if ref.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: ref.String(), Valid: true}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
