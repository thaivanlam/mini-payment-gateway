// Package http is the HTTP delivery layer: DTOs, middleware, handlers and the
// one place where domain errors become status codes.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/idempotency"
	"github.com/thaivanlam/mini-payment-gateway/internal/secrets"
)

// ErrorType is the coarse category of an error, mirroring the vocabulary card
// processors use so client libraries can branch on it.
type ErrorType string

const (
	TypeValidation     ErrorType = "validation_error"
	TypeAuthentication ErrorType = "authentication_error"
	TypeCard           ErrorType = "card_error"
	TypeIdempotency    ErrorType = "idempotency_error"
	TypeRateLimit      ErrorType = "rate_limit_error"
	TypeAPI            ErrorType = "api_error"
)

// ErrorBody is the payload of every error response.
type ErrorBody struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Type      ErrorType `json:"type"`
	RequestID string    `json:"request_id,omitempty"`
	Field     string    `json:"field,omitempty"`
}

// ErrorResponse is the envelope of every error response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// writeJSON serialises v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already on the wire; all that is left is a log.
		slog.Error("encode response", "error", err)
	}
}

// writeError maps an error to a status code and writes the envelope.
//
// This function is the only place in the codebase that turns an error into a
// status code. Handlers return errors; they never choose a number. That is why
// a new domain error cannot silently end up as a 500 in one handler and a 400
// in another.
func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	status, body := mapError(err)
	body.RequestID = RequestIDFrom(ctx)

	if status >= 500 {
		slog.ErrorContext(ctx, "request failed", "error", err, "code", body.Code, "status", status)
	} else {
		slog.InfoContext(ctx, "request rejected", "error", err, "code", body.Code, "status", status)
	}
	writeJSON(w, status, ErrorResponse{Error: body})
}

func mapError(err error) (int, ErrorBody) {
	// Card declines carry the acquirer's own reason code.
	var declined *domain.DeclinedError
	if errors.As(err, &declined) {
		return http.StatusPaymentRequired, ErrorBody{
			Code:    string(declined.Code),
			Message: declined.Message,
			Type:    TypeCard,
		}
	}

	// Field-level validation failures name the offending field.
	var invalid *domain.ValidationError
	if errors.As(err, &invalid) {
		return http.StatusBadRequest, ErrorBody{
			Code:    "invalid_request",
			Message: invalid.Message,
			Type:    TypeValidation,
			Field:   invalid.Field,
		}
	}

	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, ErrorBody{
			Code: "resource_not_found", Message: "The requested resource does not exist.", Type: TypeValidation}

	case errors.Is(err, domain.ErrUnauthenticated), errors.Is(err, secrets.ErrDecrypt):
		return http.StatusUnauthorized, ErrorBody{
			Code: "authentication_failed", Message: "The request signature could not be verified.", Type: TypeAuthentication}

	case errors.Is(err, domain.ErrMerchantSuspended):
		return http.StatusForbidden, ErrorBody{
			Code: "merchant_suspended", Message: "This merchant account is suspended.", Type: TypeAuthentication}

	case errors.Is(err, domain.ErrDuplicateReference):
		return http.StatusConflict, ErrorBody{
			Code: "duplicate_reference", Message: "A transaction with this reference already exists.", Type: TypeValidation}

	case errors.Is(err, domain.ErrInvalidStateTransition):
		return http.StatusConflict, ErrorBody{
			Code: "invalid_state", Message: "The transaction is not in a state that allows this operation.", Type: TypeValidation}

	case errors.Is(err, domain.ErrAuthorizationExpired):
		return http.StatusConflict, ErrorBody{
			Code: "authorization_expired", Message: "The authorization is older than 7 days and can no longer be captured.", Type: TypeValidation}

	case errors.Is(err, domain.ErrCaptureExceedsAuthlzd):
		return http.StatusUnprocessableEntity, ErrorBody{
			Code: "capture_exceeds_authorized", Message: "The capture amount exceeds the authorized amount.", Type: TypeValidation}

	case errors.Is(err, domain.ErrRefundExceedsCaptured):
		return http.StatusUnprocessableEntity, ErrorBody{
			Code: "refund_exceeds_captured", Message: "The refund amount exceeds the captured amount.", Type: TypeValidation}

	case errors.Is(err, domain.ErrConcurrentModification):
		return http.StatusConflict, ErrorBody{
			Code: "concurrent_modification", Message: "The transaction was modified by another request. Retry.", Type: TypeAPI}

	case errors.Is(err, idempotency.ErrInFlight):
		return http.StatusConflict, ErrorBody{
			Code: "request_in_progress", Message: "A request with this Idempotency-Key is still in progress. Retry shortly.", Type: TypeIdempotency}

	case errors.Is(err, idempotency.ErrKeyReuse):
		return http.StatusUnprocessableEntity, ErrorBody{
			Code: "idempotency_key_reuse", Message: "This Idempotency-Key was already used with a different request body.", Type: TypeIdempotency}

	case errors.Is(err, domain.ErrRateLimited):
		return http.StatusTooManyRequests, ErrorBody{
			Code: "rate_limit_exceeded", Message: "Too many requests. Slow down.", Type: TypeRateLimit}

	case errors.Is(err, domain.ErrAcquirerUnavailable):
		return http.StatusServiceUnavailable, ErrorBody{
			Code: "acquirer_unavailable", Message: "The card processor did not respond. The outcome is unknown; retry with the same Idempotency-Key.", Type: TypeAPI}

	case errors.Is(err, domain.ErrInvalidAmount),
		errors.Is(err, domain.ErrCurrencyMismatch),
		errors.Is(err, domain.ErrUnsupportedCurrency),
		errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest, ErrorBody{
			Code: "invalid_request", Message: err.Error(), Type: TypeValidation}

	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, ErrorBody{
			Code: "request_cancelled", Message: "The request was cancelled or timed out.", Type: TypeAPI}
	}

	// Anything unmapped is a bug on our side: never leak the internal message.
	return http.StatusInternalServerError, ErrorBody{
		Code: "internal_error", Message: "An unexpected error occurred.", Type: TypeAPI}
}
