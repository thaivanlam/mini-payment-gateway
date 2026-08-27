package http

import (
	"context"
	"net/http"
	"strconv"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/ratelimit"
)

// missingIdempotencyKey is the error for a money-moving POST without a key.
// Requiring the header rather than defaulting to "no idempotency" is
// deliberate: a client that forgot it finds out on the first call, not after
// the first double charge.
func missingIdempotencyKey() error {
	return domain.Invalid(IdempotencyKeyHeader, "header is required for this endpoint")
}

// RateLimiter is the port the middleware needs.
type RateLimiter interface {
	Allow(ctx context.Context, subject string) (ratelimit.Result, error)
}

// RateLimit caps the request rate per merchant and advertises the budget in
// the standard headers so clients can back off before being refused.
func RateLimit(limiter RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			merchant := MerchantFrom(ctx)
			if merchant == nil {
				next.ServeHTTP(w, r)
				return
			}

			result, err := limiter.Allow(ctx, merchant.ID.String())
			if err != nil {
				// Redis being down must not take payments down with it: fail
				// open and log. The alternative refuses real money movement to
				// protect against a load problem we are not currently having.
				LoggerFrom(ctx).ErrorContext(ctx, "rate limiter unavailable, failing open", "error", err)
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(result.Limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))

			if !result.Allowed {
				retryAfter := int(result.RetryAfter.Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeError(ctx, w, domain.ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
