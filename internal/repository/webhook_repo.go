package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
)

// WebhookRepo reads and writes the delivery outbox.
type WebhookRepo struct{}

// NewWebhookRepo builds a WebhookRepo.
func NewWebhookRepo() *WebhookRepo { return &WebhookRepo{} }

const webhookColumns = `
	id, merchant_id, transaction_id, event_type, payload, status,
	attempt_count, next_attempt_at, COALESCE(last_error, ''), created_at, updated_at`

// Create appends a delivery. It is called inside the same database transaction
// as the money movement it announces: the outbox pattern, so a committed
// capture always has a webhook queued and a rolled back one never does.
func (r *WebhookRepo) Create(ctx context.Context, q Querier, d *domain.WebhookDelivery) error {
	const query = `
		INSERT INTO webhook_deliveries (
			id, merchant_id, transaction_id, event_type, payload,
			status, attempt_count, next_attempt_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	_, err := q.Exec(ctx, query,
		d.ID, d.MerchantID, d.TransactionID, string(d.EventType), d.Payload,
		string(d.Status), d.AttemptCount, d.NextAttemptAt, d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert webhook delivery: %w", err)
	}
	return nil
}

// ClaimDue atomically claims up to limit due deliveries for this worker.
//
// FOR UPDATE SKIP LOCKED is what makes several worker processes safe: each one
// locks the rows it takes and steps over rows another worker already holds,
// instead of queueing behind them. The status flip to 'delivering' in the same
// statement means a crashed worker's rows are visible as stuck rather than
// silently retried by everyone at once.
func (r *WebhookRepo) ClaimDue(ctx context.Context, q Querier, limit int, now time.Time) ([]*domain.WebhookDelivery, error) {
	query := `
		WITH due AS (
			SELECT id
			  FROM webhook_deliveries
			 WHERE status IN ('pending', 'failed')
			   AND next_attempt_at <= $1
			 ORDER BY next_attempt_at
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE webhook_deliveries w
		   SET status = 'delivering', updated_at = now()
		  FROM due
		 WHERE w.id = due.id
		RETURNING w.id, w.merchant_id, w.transaction_id, w.event_type, w.payload, w.status,
		          w.attempt_count, w.next_attempt_at, COALESCE(w.last_error, ''), w.created_at, w.updated_at`

	rows, err := q.Query(ctx, query, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim due webhooks: %w", err)
	}
	defer rows.Close()

	var out []*domain.WebhookDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateAttempt persists the outcome of one delivery attempt.
func (r *WebhookRepo) UpdateAttempt(ctx context.Context, q Querier, d *domain.WebhookDelivery) error {
	const query = `
		UPDATE webhook_deliveries
		   SET status          = $1,
		       attempt_count   = $2,
		       next_attempt_at = $3,
		       last_error      = NULLIF($4, ''),
		       updated_at      = now()
		 WHERE id = $5`

	tag, err := q.Exec(ctx, query,
		string(d.Status), d.AttemptCount, d.NextAttemptAt, d.LastError, d.ID)
	if err != nil {
		return fmt.Errorf("update webhook delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ReleaseStale returns deliveries stuck in 'delivering' back to the queue.
// A worker killed mid-flight leaves its rows claimed; this is the janitor that
// makes "kill the worker, lose no job" true.
func (r *WebhookRepo) ReleaseStale(ctx context.Context, q Querier, olderThan time.Duration, now time.Time) (int64, error) {
	const query = `
		UPDATE webhook_deliveries
		   SET status = 'failed', next_attempt_at = $1, updated_at = now()
		 WHERE status = 'delivering'
		   AND updated_at < $2`

	tag, err := q.Exec(ctx, query, now, now.Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("release stale webhooks: %w", err)
	}
	return tag.RowsAffected(), nil
}

// GetByID loads one delivery.
func (r *WebhookRepo) GetByID(ctx context.Context, q Querier, id uuid.UUID) (*domain.WebhookDelivery, error) {
	query := `SELECT ` + webhookColumns + ` FROM webhook_deliveries WHERE id = $1`
	d, err := scanDelivery(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// ListByTransaction returns the deliveries queued for one transaction.
func (r *WebhookRepo) ListByTransaction(ctx context.Context, q Querier, txID uuid.UUID) ([]*domain.WebhookDelivery, error) {
	query := `SELECT ` + webhookColumns + ` FROM webhook_deliveries WHERE transaction_id = $1 ORDER BY created_at`
	rows, err := q.Query(ctx, query, txID)
	if err != nil {
		return nil, fmt.Errorf("list webhook deliveries: %w", err)
	}
	defer rows.Close()

	var out []*domain.WebhookDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDelivery(row rowScanner) (*domain.WebhookDelivery, error) {
	var (
		d         domain.WebhookDelivery
		eventType string
		status    string
	)
	err := row.Scan(
		&d.ID, &d.MerchantID, &d.TransactionID, &eventType, &d.Payload, &status,
		&d.AttemptCount, &d.NextAttemptAt, &d.LastError, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan webhook delivery: %w", err)
	}
	d.EventType = domain.WebhookEvent(eventType)
	d.Status = domain.DeliveryStatus(status)
	return &d, nil
}
