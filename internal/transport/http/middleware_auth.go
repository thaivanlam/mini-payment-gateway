package http

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// Authentication headers.
const (
	HeaderAPIKey    = "X-Api-Key"
	HeaderTimestamp = "X-Timestamp"
	HeaderSignature = "X-Signature"
)

// Authenticator is the port this middleware needs. It is declared here, at the
// point of use, rather than in the service package: the HTTP layer states what
// it requires, and any implementation satisfying it will do -- which is what
// makes the middleware testable without a database.
type Authenticator interface {
	Authenticate(ctx context.Context, req service.SignedRequest) (*domain.Merchant, error)
}

// Auth verifies the HMAC signature of merchant-facing requests.
//
// The signature covers timestamp + method + path + body, so a captured request
// cannot be replayed against another endpoint, with another body, or later than
// the clock-skew window allows.
func Auth(auth Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			body, err := readAndRestoreBody(w, r)
			if err != nil {
				writeError(ctx, w, err)
				return
			}

			merchant, err := auth.Authenticate(ctx, service.SignedRequest{
				APIKey:    r.Header.Get(HeaderAPIKey),
				Timestamp: r.Header.Get(HeaderTimestamp),
				Signature: r.Header.Get(HeaderSignature),
				Method:    r.Method,
				Path:      r.URL.Path,
				Body:      body,
			})
			if err != nil {
				writeError(ctx, w, err)
				return
			}

			ctx = context.WithValue(ctx, ctxKeyMerchant, merchant)
			ctx = context.WithValue(ctx, ctxKeyRawBody, body)
			ctx = withLogger(ctx, LoggerFrom(ctx).With("merchant_id", merchant.ID.String()))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MerchantFrom returns the authenticated merchant.
func MerchantFrom(ctx context.Context) *domain.Merchant {
	m, _ := ctx.Value(ctxKeyMerchant).(*domain.Merchant)
	return m
}

// rawBodyFrom returns the body buffered by the auth middleware, so the
// idempotency middleware can fingerprint it without reading the stream twice.
func rawBodyFrom(ctx context.Context) []byte {
	b, _ := ctx.Value(ctxKeyRawBody).([]byte)
	return b
}

// AdminAuth guards the merchant-provisioning endpoint with a static token.
//
// A shared bearer token is the right size for an internal admin endpoint in a
// project this size; real provisioning would sit behind an operator console
// with per-user identities and an audit trail.
func AdminAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			// Constant-time compare, for the same reason as the HMAC check.
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				writeError(r.Context(), w, fmt.Errorf("%w: invalid admin token", domain.ErrUnauthenticated))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func readAndRestoreBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, domain.Invalid("body", "could not be read")
	}
	// Hand the handler a fresh reader over the same bytes.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
