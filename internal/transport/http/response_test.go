package http

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/idempotency"
	"github.com/thaivanlam/mini-payment-gateway/internal/secrets"
)

// TestMapError pins the whole error -> status table. Everything the API can
// answer is decided here and nowhere else, so this test is the contract.
func TestMapError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantType   ErrorType
	}{
		{
			name:       "card decline",
			err:        domain.NewDeclinedError(domain.DeclineInsufficientFunds),
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "insufficient_funds",
			wantType:   TypeCard,
		},
		{
			name:       "field validation",
			err:        domain.Invalid("amount", "must be greater than zero"),
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
			wantType:   TypeValidation,
		},
		{
			name:       "not found",
			err:        domain.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "resource_not_found",
			wantType:   TypeValidation,
		},
		{
			name:       "unauthenticated",
			err:        fmt.Errorf("wrapped: %w", domain.ErrUnauthenticated),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "authentication_failed",
			wantType:   TypeAuthentication,
		},
		{
			name:       "undecryptable secret is an auth failure, not a 500",
			err:        secrets.ErrDecrypt,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "authentication_failed",
			wantType:   TypeAuthentication,
		},
		{
			name:       "suspended merchant",
			err:        domain.ErrMerchantSuspended,
			wantStatus: http.StatusForbidden,
			wantCode:   "merchant_suspended",
			wantType:   TypeAuthentication,
		},
		{
			name:       "duplicate reference",
			err:        domain.ErrDuplicateReference,
			wantStatus: http.StatusConflict,
			wantCode:   "duplicate_reference",
			wantType:   TypeValidation,
		},
		{
			name:       "invalid state transition",
			err:        fmt.Errorf("%w: captured -> authorized", domain.ErrInvalidStateTransition),
			wantStatus: http.StatusConflict,
			wantCode:   "invalid_state",
			wantType:   TypeValidation,
		},
		{
			name:       "expired authorization",
			err:        domain.ErrAuthorizationExpired,
			wantStatus: http.StatusConflict,
			wantCode:   "authorization_expired",
			wantType:   TypeValidation,
		},
		{
			name:       "capture too large",
			err:        domain.ErrCaptureExceedsAuthlzd,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "capture_exceeds_authorized",
			wantType:   TypeValidation,
		},
		{
			name:       "refund too large",
			err:        domain.ErrRefundExceedsCaptured,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "refund_exceeds_captured",
			wantType:   TypeValidation,
		},
		{
			name:       "concurrent modification",
			err:        domain.ErrConcurrentModification,
			wantStatus: http.StatusConflict,
			wantCode:   "concurrent_modification",
			wantType:   TypeAPI,
		},
		{
			name:       "idempotent request in flight",
			err:        idempotency.ErrInFlight,
			wantStatus: http.StatusConflict,
			wantCode:   "request_in_progress",
			wantType:   TypeIdempotency,
		},
		{
			name:       "idempotency key reuse",
			err:        idempotency.ErrKeyReuse,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "idempotency_key_reuse",
			wantType:   TypeIdempotency,
		},
		{
			name:       "rate limited",
			err:        domain.ErrRateLimited,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "rate_limit_exceeded",
			wantType:   TypeRateLimit,
		},
		{
			name:       "acquirer unavailable",
			err:        fmt.Errorf("authorize: %w", domain.ErrAcquirerUnavailable),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "acquirer_unavailable",
			wantType:   TypeAPI,
		},
		{
			name:       "cancelled context",
			err:        context.Canceled,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "request_cancelled",
			wantType:   TypeAPI,
		},
		{
			name:       "unmapped error is a 500 with no internal detail",
			err:        fmt.Errorf("pq: relation \"transactions\" does not exist"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
			wantType:   TypeAPI,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := mapError(tc.err)
			assert.Equal(t, tc.wantStatus, status)
			assert.Equal(t, tc.wantCode, body.Code)
			assert.Equal(t, tc.wantType, body.Type)
			assert.NotEmpty(t, body.Message)
		})
	}
}

func TestMapErrorNeverLeaksInternalDetail(t *testing.T) {
	_, body := mapError(fmt.Errorf("dial tcp 10.0.0.5:5432: connection refused"))

	assert.Equal(t, "An unexpected error occurred.", body.Message)
	assert.NotContains(t, body.Message, "10.0.0.5")
}

func TestValidationErrorCarriesField(t *testing.T) {
	_, body := mapError(domain.Invalid("card.number", "must be 12 to 19 digits"))
	assert.Equal(t, "card.number", body.Field)
	assert.Equal(t, "must be 12 to 19 digits", body.Message)
}
