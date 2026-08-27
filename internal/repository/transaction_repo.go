package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
)

// TransactionRepo reads and writes the transactions table.
type TransactionRepo struct{}

// NewTransactionRepo builds a TransactionRepo.
func NewTransactionRepo() *TransactionRepo { return &TransactionRepo{} }

const txnColumns = `
	id, merchant_id, reference, amount, currency, status,
	captured_amount, refunded_amount,
	COALESCE(card_last4, ''), COALESCE(card_brand, ''),
	COALESCE(acquirer_ref, ''), COALESCE(failure_code, ''),
	metadata, version,
	authorized_at, captured_at, settled_at, created_at, updated_at`

// Create inserts a new transaction.
func (r *TransactionRepo) Create(ctx context.Context, q Querier, t *domain.Transaction) error {
	metadata, err := json.Marshal(nonNilMetadata(t.Metadata))
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	const query = `
		INSERT INTO transactions (
			id, merchant_id, reference, amount, currency, status,
			captured_amount, refunded_amount, metadata, version, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	_, err = q.Exec(ctx, query,
		t.ID, t.MerchantID, t.Reference, int64(t.Amount), string(t.Currency), string(t.Status),
		int64(t.CapturedAmount), int64(t.RefundedAmount), metadata, t.Version, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		if constraint, ok := isUniqueViolation(err); ok && constraint == "transactions_merchant_reference_key" {
			return fmt.Errorf("%w: reference %q", domain.ErrDuplicateReference, t.Reference)
		}
		if constraint, ok := isCheckViolation(err); ok {
			return fmt.Errorf("%w: %s", domain.ErrValidation, constraint)
		}
		return fmt.Errorf("insert transaction: %w", err)
	}
	return nil
}

// GetByID loads a transaction without locking it.
func (r *TransactionRepo) GetByID(ctx context.Context, q Querier, id uuid.UUID) (*domain.Transaction, error) {
	query := `SELECT ` + txnColumns + ` FROM transactions WHERE id = $1`
	return scanTransaction(q.QueryRow(ctx, query, id))
}

// GetByMerchantAndID scopes the lookup to one merchant, so a merchant can
// never read another merchant's transaction by guessing an id.
func (r *TransactionRepo) GetByMerchantAndID(ctx context.Context, q Querier, merchantID, id uuid.UUID) (*domain.Transaction, error) {
	query := `SELECT ` + txnColumns + ` FROM transactions WHERE id = $1 AND merchant_id = $2`
	return scanTransaction(q.QueryRow(ctx, query, id, merchantID))
}

// GetByReference finds a transaction by the merchant's own order id.
func (r *TransactionRepo) GetByReference(ctx context.Context, q Querier, merchantID uuid.UUID, reference string) (*domain.Transaction, error) {
	query := `SELECT ` + txnColumns + ` FROM transactions WHERE merchant_id = $1 AND reference = $2`
	return scanTransaction(q.QueryRow(ctx, query, merchantID, reference))
}

// GetForUpdate loads a transaction and holds a row lock until the surrounding
// transaction ends. This is what serialises two concurrent captures: the second
// one blocks here, and by the time it proceeds it sees the first one's writes.
func (r *TransactionRepo) GetForUpdate(ctx context.Context, q Querier, id uuid.UUID) (*domain.Transaction, error) {
	query := `SELECT ` + txnColumns + ` FROM transactions WHERE id = $1 FOR UPDATE`
	return scanTransaction(q.QueryRow(ctx, query, id))
}

// Update writes the mutable fields back, guarded by the version column.
//
// The row lock from GetForUpdate already serialises writers inside a
// transaction; the version check is the second belt: it also catches a writer
// that read the row without locking it. rows_affected == 0 means someone else
// moved the transaction on, and the caller must not assume its in-memory copy
// is still true.
func (r *TransactionRepo) Update(ctx context.Context, q Querier, t *domain.Transaction) error {
	const query = `
		UPDATE transactions
		   SET status          = $1,
		       captured_amount = $2,
		       refunded_amount = $3,
		       card_last4      = NULLIF($4, ''),
		       card_brand      = NULLIF($5, ''),
		       acquirer_ref    = NULLIF($6, ''),
		       failure_code    = NULLIF($7, ''),
		       authorized_at   = $8,
		       captured_at     = $9,
		       settled_at      = $10,
		       version         = version + 1,
		       updated_at      = now()
		 WHERE id = $11 AND version = $12
		RETURNING version, updated_at`

	var version int
	var updatedAt time.Time
	err := q.QueryRow(ctx, query,
		string(t.Status), int64(t.CapturedAmount), int64(t.RefundedAmount),
		t.CardLast4, t.CardBrand, t.AcquirerRef, t.FailureCode,
		t.AuthorizedAt, t.CapturedAt, t.SettledAt,
		t.ID, t.Version,
	).Scan(&version, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: transaction %s version %d", domain.ErrConcurrentModification, t.ID, t.Version)
		}
		if constraint, ok := isCheckViolation(err); ok {
			return fmt.Errorf("%w: %s", domain.ErrValidation, constraint)
		}
		return fmt.Errorf("update transaction: %w", err)
	}
	t.Version = version
	t.UpdatedAt = updatedAt
	return nil
}

// ListFilter narrows a transaction listing.
type ListFilter struct {
	MerchantID uuid.UUID
	Status     *domain.Status
	From       *time.Time
	To         *time.Time
	// Cursor is the (created_at, id) pair of the last row of the previous page.
	CursorTime *time.Time
	CursorID   *uuid.UUID
	Limit      int
}

// List returns one page of transactions, newest first.
//
// Pagination is cursor based rather than OFFSET based: OFFSET makes the
// database walk and discard every skipped row, so page 500 costs 500 pages of
// work, and a row inserted meanwhile shifts every later page. Comparing the
// (created_at, id) tuple against the index is O(log n) per page and stable.
func (r *TransactionRepo) List(ctx context.Context, q Querier, f ListFilter) ([]*domain.Transaction, error) {
	// The handler asks for one row more than the page size so it can tell
	// whether another page exists, so the ceiling here is above the API maximum.
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 201 {
		f.Limit = 201
	}
	var status *string
	if f.Status != nil {
		s := string(*f.Status)
		status = &s
	}

	query := `
		SELECT ` + txnColumns + `
		  FROM transactions
		 WHERE merchant_id = $1
		   AND ($2::text IS NULL OR status = $2)
		   AND ($3::timestamptz IS NULL OR created_at >= $3)
		   AND ($4::timestamptz IS NULL OR created_at <= $4)
		   AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6::uuid))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $7`

	rows, err := q.Query(ctx, query, f.MerchantID, status, f.From, f.To, f.CursorTime, f.CursorID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()

	var out []*domain.Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	return out, nil
}

// ListCapturedBetween returns every transaction captured in [from, to), used by
// the reconciliation job.
func (r *TransactionRepo) ListCapturedBetween(ctx context.Context, q Querier, from, to time.Time) ([]*domain.Transaction, error) {
	query := `
		SELECT ` + txnColumns + `
		  FROM transactions
		 WHERE captured_at >= $1
		   AND captured_at <  $2
		   AND status IN ('captured', 'settled', 'refunded')
		 ORDER BY merchant_id, captured_at`

	rows, err := q.Query(ctx, query, from, to)
	if err != nil {
		return nil, fmt.Errorf("list captured transactions: %w", err)
	}
	defer rows.Close()

	var out []*domain.Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list captured transactions: %w", err)
	}
	return out, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTransaction(row rowScanner) (*domain.Transaction, error) {
	var (
		t        domain.Transaction
		amount   int64
		captured int64
		refunded int64
		currency string
		status   string
		metadata []byte
	)
	err := row.Scan(
		&t.ID, &t.MerchantID, &t.Reference, &amount, &currency, &status,
		&captured, &refunded,
		&t.CardLast4, &t.CardBrand, &t.AcquirerRef, &t.FailureCode,
		&metadata, &t.Version,
		&t.AuthorizedAt, &t.CapturedAt, &t.SettledAt, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan transaction: %w", err)
	}

	t.Amount = moneyAmount(amount)
	t.CapturedAmount = moneyAmount(captured)
	t.RefundedAmount = moneyAmount(refunded)
	t.Currency = currencyOf(currency)
	t.Status = domain.Status(status)

	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &t.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	return &t, nil
}

func nonNilMetadata(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
