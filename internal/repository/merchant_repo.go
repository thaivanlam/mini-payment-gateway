package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
)

// MerchantRepo reads and writes the merchants table.
type MerchantRepo struct{}

// NewMerchantRepo builds a MerchantRepo.
func NewMerchantRepo() *MerchantRepo { return &MerchantRepo{} }

const merchantColumns = `
	id, name, email, api_key, api_secret_enc, COALESCE(webhook_url, ''),
	webhook_secret_enc, status, created_at, updated_at`

// Create inserts a merchant.
func (r *MerchantRepo) Create(ctx context.Context, q Querier, m *domain.Merchant) error {
	const query = `
		INSERT INTO merchants (
			id, name, email, api_key, api_secret_enc,
			webhook_url, webhook_secret_enc, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10)`

	_, err := q.Exec(ctx, query,
		m.ID, m.Name, m.Email, m.APIKey, m.APISecretEnc,
		m.WebhookURL, m.WebhookSecretEnc, string(m.Status), m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		if constraint, ok := isUniqueViolation(err); ok {
			switch constraint {
			case "merchants_email_key":
				return fmt.Errorf("%w: email already registered", domain.ErrValidation)
			case "merchants_api_key_key":
				return fmt.Errorf("%w: api key collision", domain.ErrValidation)
			}
		}
		return fmt.Errorf("insert merchant: %w", err)
	}
	return nil
}

// GetByID loads a merchant by primary key.
func (r *MerchantRepo) GetByID(ctx context.Context, q Querier, id uuid.UUID) (*domain.Merchant, error) {
	query := `SELECT ` + merchantColumns + ` FROM merchants WHERE id = $1`
	return scanMerchant(q.QueryRow(ctx, query, id))
}

// GetByAPIKey loads a merchant by its public API key. This is on the hot path
// of every authenticated request; the api_key column is UNIQUE, so the lookup
// is a single index probe.
func (r *MerchantRepo) GetByAPIKey(ctx context.Context, q Querier, apiKey string) (*domain.Merchant, error) {
	query := `SELECT ` + merchantColumns + ` FROM merchants WHERE api_key = $1`
	return scanMerchant(q.QueryRow(ctx, query, apiKey))
}

// UpdateWebhook changes the callback URL of a merchant.
func (r *MerchantRepo) UpdateWebhook(ctx context.Context, q Querier, id uuid.UUID, url string) error {
	const query = `
		UPDATE merchants
		   SET webhook_url = NULLIF($2, ''), updated_at = now()
		 WHERE id = $1`
	tag, err := q.Exec(ctx, query, id, url)
	if err != nil {
		return fmt.Errorf("update merchant webhook: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func scanMerchant(row pgx.Row) (*domain.Merchant, error) {
	var m domain.Merchant
	var status string
	err := row.Scan(
		&m.ID, &m.Name, &m.Email, &m.APIKey, &m.APISecretEnc,
		&m.WebhookURL, &m.WebhookSecretEnc, &status, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan merchant: %w", err)
	}
	m.Status = domain.MerchantStatus(status)
	return &m, nil
}
