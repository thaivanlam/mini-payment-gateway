package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// LedgerRepo appends to and reads from the double-entry journal. There is
// deliberately no Update and no Delete: a correction is a new entry group.
type LedgerRepo struct{}

// NewLedgerRepo builds a LedgerRepo.
func NewLedgerRepo() *LedgerRepo { return &LedgerRepo{} }

const ledgerColumns = `
	id, entry_group_id, transaction_id, account, merchant_id,
	direction, amount, currency, event_type, created_at`

// InsertGroup appends a whole entry group. It re-validates the invariant right
// before the write: nothing unbalanced reaches the journal even if a caller
// built the group by hand.
func (r *LedgerRepo) InsertGroup(ctx context.Context, q Querier, group domain.EntryGroup) error {
	if err := group.Validate(); err != nil {
		return err
	}

	const query = `
		INSERT INTO ledger_entries (
			entry_group_id, transaction_id, account, merchant_id,
			direction, amount, currency, event_type, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	for _, e := range group.Entries {
		_, err := q.Exec(ctx, query,
			e.EntryGroupID, e.TransactionID, e.Account, e.MerchantID,
			string(e.Direction), int64(e.Amount), string(e.Currency),
			string(e.EventType), e.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert ledger entry (%s %s): %w", e.Account, e.Direction, err)
		}
	}
	return nil
}

// Balance computes a merchant balance by folding the journal. No balance is
// ever stored: the number below is the only definition of "what we owe".
func (r *LedgerRepo) Balance(ctx context.Context, q Querier, merchantID uuid.UUID, currency money.Currency) (domain.Balance, error) {
	const query = `
		SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
		       COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
		  FROM ledger_entries
		 WHERE merchant_id = $1
		   AND account     = $2
		   AND currency    = $3`

	account := domain.MerchantPayableAccount(merchantID)
	var debits, credits int64
	if err := q.QueryRow(ctx, query, merchantID, account, string(currency)).Scan(&debits, &credits); err != nil {
		return domain.Balance{}, fmt.Errorf("compute balance: %w", err)
	}
	return domain.Balance{
		Account:  account,
		Currency: currency,
		Debits:   moneyAmount(debits),
		Credits:  moneyAmount(credits),
	}, nil
}

// EntryFilter narrows a journal listing.
type EntryFilter struct {
	MerchantID uuid.UUID
	Account    string
	From       *time.Time
	To         *time.Time
	CursorID   *int64
	Limit      int
}

// ListEntries returns one page of journal lines, newest first. The BIGSERIAL id
// is monotonic, so it doubles as a stable cursor.
func (r *LedgerRepo) ListEntries(ctx context.Context, q Querier, f EntryFilter) ([]domain.LedgerEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 201 {
		f.Limit = 201
	}
	var account *string
	if f.Account != "" {
		account = &f.Account
	}

	query := `
		SELECT ` + ledgerColumns + `
		  FROM ledger_entries
		 WHERE merchant_id = $1
		   AND ($2::text IS NULL OR account = $2)
		   AND ($3::timestamptz IS NULL OR created_at >= $3)
		   AND ($4::timestamptz IS NULL OR created_at <= $4)
		   AND ($5::bigint IS NULL OR id < $5)
		 ORDER BY id DESC
		 LIMIT $6`

	rows, err := q.Query(ctx, query, f.MerchantID, account, f.From, f.To, f.CursorID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list ledger entries: %w", err)
	}
	defer rows.Close()

	var out []domain.LedgerEntry
	for rows.Next() {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ledger entries: %w", err)
	}
	return out, nil
}

// ListByTransaction returns every journal line of one transaction, oldest first.
func (r *LedgerRepo) ListByTransaction(ctx context.Context, q Querier, txID uuid.UUID) ([]domain.LedgerEntry, error) {
	query := `SELECT ` + ledgerColumns + ` FROM ledger_entries WHERE transaction_id = $1 ORDER BY id`
	rows, err := q.Query(ctx, query, txID)
	if err != nil {
		return nil, fmt.Errorf("list entries by transaction: %w", err)
	}
	defer rows.Close()

	var out []domain.LedgerEntry
	for rows.Next() {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountEntryGroups returns how many distinct accounting events a transaction
// produced. The concurrency test asserts that a double capture produces one.
func (r *LedgerRepo) CountEntryGroups(ctx context.Context, q Querier, txID uuid.UUID, event domain.EventType) (int, error) {
	const query = `
		SELECT COUNT(DISTINCT entry_group_id)
		  FROM ledger_entries
		 WHERE transaction_id = $1 AND event_type = $2`
	var n int
	if err := q.QueryRow(ctx, query, txID, string(event)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count entry groups: %w", err)
	}
	return n, nil
}

// UnbalancedGroup names one entry group whose debits and credits disagree.
type UnbalancedGroup struct {
	EntryGroupID uuid.UUID
	Debits       money.Amount
	Credits      money.Amount
}

// FindUnbalancedGroups scans the whole journal for broken groups. It is the
// invariant check the integration tests run after every scenario, and the
// reconciliation job runs daily.
func (r *LedgerRepo) FindUnbalancedGroups(ctx context.Context, q Querier) ([]UnbalancedGroup, error) {
	const query = `
		SELECT entry_group_id,
		       COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)  AS debits,
		       COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0) AS credits
		  FROM ledger_entries
		 GROUP BY entry_group_id
		HAVING COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)
		    <> COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)`

	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("find unbalanced groups: %w", err)
	}
	defer rows.Close()

	var out []UnbalancedGroup
	for rows.Next() {
		var g UnbalancedGroup
		var debits, credits int64
		if err := rows.Scan(&g.EntryGroupID, &debits, &credits); err != nil {
			return nil, fmt.Errorf("scan unbalanced group: %w", err)
		}
		g.Debits, g.Credits = moneyAmount(debits), moneyAmount(credits)
		out = append(out, g)
	}
	return out, rows.Err()
}

// DailyTotals is the journal side of the reconciliation comparison.
type DailyTotals struct {
	MerchantID uuid.UUID
	Currency   money.Currency
	Captured   money.Amount
	Refunded   money.Amount
	Fees       money.Amount
}

// AggregateByMerchant sums the journal per merchant for the transactions
// captured in [from, to).
//
// The window is applied to transactions.captured_at, not to the entry
// timestamp, so both sides of the reconciliation slice the day the same way. A
// refund booked today against yesterday's capture belongs to yesterday's
// settlement batch; filtering on the entry timestamp would report that as a
// discrepancy on both days.
func (r *LedgerRepo) AggregateByMerchant(ctx context.Context, q Querier, from, to time.Time) ([]DailyTotals, error) {
	const query = `
		SELECT e.merchant_id,
		       e.currency,
		       COALESCE(SUM(e.amount) FILTER (
		           WHERE e.event_type = 'capture' AND e.account = $3 AND e.direction = 'debit'), 0)  AS captured,
		       COALESCE(SUM(e.amount) FILTER (
		           WHERE e.event_type = 'refund'  AND e.account = $3 AND e.direction = 'credit'), 0) AS refunded,
		       COALESCE(SUM(e.amount) FILTER (
		           WHERE e.event_type = 'fee'     AND e.account = $4 AND e.direction = 'credit'), 0) AS fees
		  FROM ledger_entries e
		  JOIN transactions   t ON t.id = e.transaction_id
		 WHERE t.captured_at >= $1
		   AND t.captured_at <  $2
		   AND e.merchant_id IS NOT NULL
		 GROUP BY e.merchant_id, e.currency
		 ORDER BY e.merchant_id`

	rows, err := q.Query(ctx, query, from, to,
		domain.AccountAcquirerReceivable, domain.AccountPlatformFeeRevenue)
	if err != nil {
		return nil, fmt.Errorf("aggregate ledger by merchant: %w", err)
	}
	defer rows.Close()

	var out []DailyTotals
	for rows.Next() {
		var t DailyTotals
		var currency string
		var captured, refunded, fees int64
		if err := rows.Scan(&t.MerchantID, &currency, &captured, &refunded, &fees); err != nil {
			return nil, fmt.Errorf("scan ledger aggregate: %w", err)
		}
		t.Currency = currencyOf(currency)
		t.Captured, t.Refunded, t.Fees = moneyAmount(captured), moneyAmount(refunded), moneyAmount(fees)
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanLedgerEntry(row rowScanner) (domain.LedgerEntry, error) {
	var (
		e         domain.LedgerEntry
		direction string
		currency  string
		eventType string
		amount    int64
	)
	err := row.Scan(
		&e.ID, &e.EntryGroupID, &e.TransactionID, &e.Account, &e.MerchantID,
		&direction, &amount, &currency, &eventType, &e.CreatedAt,
	)
	if err != nil {
		return domain.LedgerEntry{}, fmt.Errorf("scan ledger entry: %w", err)
	}
	e.Direction = domain.Direction(direction)
	e.Amount = moneyAmount(amount)
	e.Currency = currencyOf(currency)
	e.EventType = domain.EventType(eventType)
	return e, nil
}
