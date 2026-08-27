package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/thaivanlam/mini-payment-gateway/internal/idempotency"
)

// IdempotencyKeyHeader is required on every money-moving POST.
const IdempotencyKeyHeader = "Idempotency-Key"

// ReplayHeader marks a response served from the idempotency store.
const ReplayHeader = "Idempotent-Replay"

// IdempotencyStore is the port the middleware needs.
type IdempotencyStore interface {
	Begin(ctx context.Context, key, fingerprint string) (*idempotency.Record, error)
	Complete(ctx context.Context, key, fingerprint string, statusCode int, contentType string, body []byte) error
	Release(ctx context.Context, key string) error
}

// Idempotency makes a retried money-moving request safe.
//
// The flow, and the reasoning behind each branch:
//
//  1. Fingerprint the body (SHA-256 of canonicalised JSON) and claim
//     "idem:<merchant>:<key>" with SET NX. The claim is atomic, so exactly one
//     of N simultaneous retries wins.
//  2. Claim won -> run the handler, then store {status, body, fingerprint}.
//  3. Claim lost, record still in progress -> 409. The first request is still
//     running; answering anything else would either duplicate the charge or
//     invent a result.
//  4. Claim lost, record complete, same fingerprint -> replay the stored
//     response byte for byte and do not run the handler.
//  5. Claim lost, different fingerprint -> 422. Same key, different body is a
//     client bug, and guessing which body was meant is worse than refusing.
//  6. Handler panicked or answered 5xx -> release the key, because the client
//     must be able to retry an outcome we could not determine.
func Idempotency(store IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			log := LoggerFrom(ctx)

			idemKey := r.Header.Get(IdempotencyKeyHeader)
			if idemKey == "" {
				writeError(ctx, w, missingIdempotencyKey())
				return
			}

			merchant := MerchantFrom(ctx)
			if merchant == nil {
				// Wiring error: this middleware must sit behind Auth.
				writeError(ctx, w, errors.New("idempotency middleware requires an authenticated merchant"))
				return
			}

			body := rawBodyFrom(ctx)
			if body == nil {
				var err error
				body, err = readAndRestoreBody(w, r)
				if err != nil {
					writeError(ctx, w, err)
					return
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			key := idempotency.Key(merchant.ID.String(), idemKey)
			fingerprint := idempotency.Fingerprint(body)

			stored, err := store.Begin(ctx, key, fingerprint)
			if err != nil {
				writeError(ctx, w, err)
				return
			}
			if stored != nil {
				log.InfoContext(ctx, "replaying idempotent response",
					"idempotency_key", idemKey, "status", stored.StatusCode)
				contentType := stored.ContentType
				if contentType == "" {
					contentType = "application/json; charset=utf-8"
				}
				w.Header().Set("Content-Type", contentType)
				w.Header().Set(ReplayHeader, "true")
				w.WriteHeader(stored.StatusCode)
				_, _ = w.Write(stored.Body)
				return
			}

			rec := &responseRecorder{ResponseWriter: w, capture: true}

			// A panic must release the key before it travels on to Recoverer.
			released := false
			defer func() {
				if p := recover(); p != nil {
					if !released {
						releaseKey(ctx, store, key, log)
					}
					panic(p)
				}
			}()

			next.ServeHTTP(rec, r)

			status := rec.statusCode()
			if status >= 500 {
				releaseKey(ctx, store, key, log)
				released = true
				return
			}

			contentType := rec.Header().Get("Content-Type")
			if err := store.Complete(ctx, key, fingerprint, status, contentType, rec.body); err != nil {
				// The response is already sent; the worst case is that a retry
				// re-runs the handler, which the database constraints then
				// reject. Log it loudly rather than failing the request.
				log.ErrorContext(ctx, "store idempotent response", "error", err, "idempotency_key", idemKey)
			}
		})
	}
}

func releaseKey(ctx context.Context, store IdempotencyStore, key string, log logHandle) {
	// Use a context detached from the request: the client may have gone away,
	// and the key must be released regardless.
	if err := store.Release(context.WithoutCancel(ctx), key); err != nil {
		log.ErrorContext(ctx, "release idempotency key", "error", err)
	}
}

// logHandle is the slice of *slog.Logger this file uses.
type logHandle interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}
