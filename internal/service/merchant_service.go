// Package service orchestrates the use cases: it owns database transactions,
// calls the acquirer, and enforces the order of operations. Business rules
// themselves live in domain; SQL lives in repository.
package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/internal/secrets"
)

// MerchantService creates merchants and authenticates their requests.
type MerchantService struct {
	db        *repository.DB
	merchants *repository.MerchantRepo
	cipher    *secrets.Cipher
	clockSkew time.Duration
	log       *slog.Logger
	now       func() time.Time
}

// NewMerchantService builds a MerchantService.
func NewMerchantService(
	db *repository.DB,
	merchants *repository.MerchantRepo,
	cipher *secrets.Cipher,
	clockSkew time.Duration,
	log *slog.Logger,
) *MerchantService {
	if log == nil {
		log = slog.Default()
	}
	return &MerchantService{
		db:        db,
		merchants: merchants,
		cipher:    cipher,
		clockSkew: clockSkew,
		log:       log,
		now:       time.Now,
	}
}

// CreateMerchantInput is what an admin supplies.
type CreateMerchantInput struct {
	Name       string
	Email      string
	WebhookURL string
}

// CreatedMerchant carries the one and only look at the plaintext secrets.
type CreatedMerchant struct {
	Merchant      *domain.Merchant
	APISecret     string
	WebhookSecret string
}

// Create registers a merchant and returns its credentials.
//
// The plaintext api_secret is returned here and never again: the database only
// holds the ciphertext, and there is no endpoint that reveals it. A merchant
// that loses it has to have it rotated, which is the property that makes the
// credential worth something.
func (s *MerchantService) Create(ctx context.Context, in CreateMerchantInput) (*CreatedMerchant, error) {
	if err := domain.ValidateMerchantInput(in.Name, in.Email, in.WebhookURL); err != nil {
		return nil, err
	}

	apiKey, err := secrets.NewAPIKey()
	if err != nil {
		return nil, err
	}
	apiSecret, err := secrets.NewAPISecret()
	if err != nil {
		return nil, err
	}
	webhookSecret, err := secrets.NewWebhookSecret()
	if err != nil {
		return nil, err
	}

	apiSecretEnc, err := s.cipher.Encrypt(apiSecret)
	if err != nil {
		return nil, fmt.Errorf("encrypt api secret: %w", err)
	}
	webhookSecretEnc, err := s.cipher.Encrypt(webhookSecret)
	if err != nil {
		return nil, fmt.Errorf("encrypt webhook secret: %w", err)
	}

	now := s.now().UTC()
	merchant := &domain.Merchant{
		ID:               uuid.New(),
		Name:             in.Name,
		Email:            in.Email,
		APIKey:           apiKey,
		APISecretEnc:     apiSecretEnc,
		WebhookURL:       in.WebhookURL,
		WebhookSecretEnc: webhookSecretEnc,
		Status:           domain.MerchantActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.merchants.Create(ctx, s.db.Pool, merchant); err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "merchant created",
		"merchant_id", merchant.ID.String(), "api_key", apiKey)

	return &CreatedMerchant{
		Merchant:      merchant,
		APISecret:     apiSecret,
		WebhookSecret: webhookSecret,
	}, nil
}

// GetByID loads a merchant.
func (s *MerchantService) GetByID(ctx context.Context, id uuid.UUID) (*domain.Merchant, error) {
	return s.merchants.GetByID(ctx, s.db.Pool, id)
}

// SignedRequest is everything the signature covers.
type SignedRequest struct {
	APIKey    string
	Timestamp string
	Signature string
	Method    string
	Path      string
	Body      []byte
}

// Authenticate verifies an HMAC-signed request and returns the merchant.
//
// Including the method and path stops a valid signature being replayed against
// a different endpoint; including the timestamp, checked against a clock-skew
// window, stops it being replayed later against the same one.
func (s *MerchantService) Authenticate(ctx context.Context, req SignedRequest) (*domain.Merchant, error) {
	if req.APIKey == "" || req.Timestamp == "" || req.Signature == "" {
		return nil, fmt.Errorf("%w: missing X-Api-Key, X-Timestamp or X-Signature", domain.ErrUnauthenticated)
	}

	ts, err := parseUnixSeconds(req.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("%w: X-Timestamp is not a unix timestamp", domain.ErrUnauthenticated)
	}
	drift := s.now().Sub(ts)
	if drift < 0 {
		drift = -drift
	}
	if drift > s.clockSkew {
		return nil, fmt.Errorf("%w: X-Timestamp drifted %s from server time", domain.ErrUnauthenticated, drift.Truncate(time.Second))
	}

	merchant, err := s.merchants.GetByAPIKey(ctx, s.db.Pool, req.APIKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: unknown api key", domain.ErrUnauthenticated)
		}
		return nil, err
	}

	secret, err := s.cipher.Decrypt(merchant.APISecretEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt api secret: %w", err)
	}

	expected := ComputeRequestSignature(secret, req.Timestamp, req.Method, req.Path, req.Body)
	// Constant-time compare: == returns early on the first differing byte and
	// leaks, through response time, how many leading bytes a guess got right.
	if !hmac.Equal([]byte(expected), []byte(req.Signature)) {
		s.log.WarnContext(ctx, "signature mismatch",
			"merchant_id", merchant.ID.String(), "method", req.Method, "path", req.Path)
		return nil, fmt.Errorf("%w: signature mismatch", domain.ErrUnauthenticated)
	}

	if !merchant.IsActive() {
		return nil, domain.ErrMerchantSuspended
	}
	return merchant, nil
}

// ComputeRequestSignature builds the value merchants put in X-Signature.
//
// The signed string is the four components joined by newline characters:
//
//	<timestamp> LF <method> LF <path> LF <body>
//
// The delimiters are not decoration. Plain concatenation is ambiguous: the pair
// (path "/a", body "b") and the pair (path "/ab", body "") produce the same
// signed bytes, so one valid signature could be presented as a signature for a
// different request. A separator that cannot appear in a timestamp, an HTTP
// method or a URL path removes that class of attack entirely. It is the same
// reason AWS SigV4 and Stripe use canonical, delimited strings rather than
// gluing fields together.
//
// Exported so the seed command and scripts/sign.sh have one definition to agree
// with.
func ComputeRequestSignature(secret, timestamp, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(signatureSeparator)
	mac.Write([]byte(method))
	mac.Write(signatureSeparator)
	mac.Write([]byte(path))
	mac.Write(signatureSeparator)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// signatureSeparator cannot appear in a timestamp, a method or a URL path.
var signatureSeparator = []byte{'\n'}

func parseUnixSeconds(raw string) (time.Time, error) {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(seconds, 0), nil
}

// WebhookSecret returns a merchant's plaintext webhook signing secret. It is
// used by the demo receiver and by nothing that faces a merchant: the secret
// never appears in an API response after the merchant was created.
func (s *MerchantService) WebhookSecret(ctx context.Context, merchantID uuid.UUID) (string, error) {
	merchant, err := s.merchants.GetByID(ctx, s.db.Pool, merchantID)
	if err != nil {
		return "", err
	}
	secret, err := s.cipher.Decrypt(merchant.WebhookSecretEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt webhook secret: %w", err)
	}
	return secret, nil
}
