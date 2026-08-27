// Package idempotency makes retrying a money-moving request safe.
//
// The contract: for a given (merchant, Idempotency-Key), the handler runs at
// most once. A retry with the same body replays the stored response; a retry
// with a different body is a client bug and is rejected rather than silently
// creating a second charge.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// State is the lifecycle of a stored idempotency record.
type State string

const (
	// StateInProgress means a request holding this key is running right now.
	StateInProgress State = "in_progress"
	// StateCompleted means the response is stored and can be replayed.
	StateCompleted State = "completed"
)

// Errors returned by the store.
var (
	// ErrInFlight means another request with the same key is still running.
	ErrInFlight = errors.New("a request with this idempotency key is in progress")
	// ErrKeyReuse means the key was reused with a different request body.
	ErrKeyReuse = errors.New("idempotency key reused with a different payload")
)

// Record is what is stored in Redis for one key.
type Record struct {
	State       State           `json:"state"`
	Fingerprint string          `json:"fingerprint"`
	StatusCode  int             `json:"status_code,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Store is the Redis-backed implementation.
type Store struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewStore builds a Store.
func NewStore(rdb *redis.Client, ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Store{rdb: rdb, ttl: ttl}
}

// Key namespaces a merchant's idempotency key. Scoping by merchant means one
// merchant cannot probe or collide with another merchant's keys.
func Key(merchantID, idempotencyKey string) string {
	return "idem:" + merchantID + ":" + idempotencyKey
}

// Begin claims the key for this request.
//
// The claim is a single SET NX, which is atomic: exactly one of N concurrent
// requests with the same key wins, and the losers are told the work is already
// in flight. It returns (nil, nil) when the caller won and must run the handler.
func (s *Store) Begin(ctx context.Context, key, fingerprint string) (*Record, error) {
	claim := Record{
		State:       StateInProgress,
		Fingerprint: fingerprint,
		CreatedAt:   time.Now().UTC(),
	}
	payload, err := json.Marshal(claim)
	if err != nil {
		return nil, fmt.Errorf("marshal idempotency claim: %w", err)
	}

	ok, err := s.rdb.SetNX(ctx, key, payload, s.ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("claim idempotency key: %w", err)
	}
	if ok {
		return nil, nil // we own the key: run the handler
	}

	existing, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		// The key expired between SETNX and GET. Treat it as in flight rather
		// than risking a second charge; the client retries.
		return nil, ErrInFlight
	}
	if existing.Fingerprint != fingerprint {
		return nil, ErrKeyReuse
	}
	if existing.State == StateInProgress {
		return nil, ErrInFlight
	}
	return existing, nil
}

// Get reads a record, returning (nil, nil) when the key is absent.
func (s *Store) Get(ctx context.Context, key string) (*Record, error) {
	raw, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read idempotency key: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("decode idempotency record: %w", err)
	}
	return &rec, nil
}

// Complete stores the response so later retries can replay it verbatim.
func (s *Store) Complete(ctx context.Context, key, fingerprint string, statusCode int, contentType string, body []byte) error {
	rec := Record{
		State:       StateCompleted,
		Fingerprint: fingerprint,
		StatusCode:  statusCode,
		ContentType: contentType,
		Body:        json.RawMessage(body),
		CreatedAt:   time.Now().UTC(),
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal idempotency record: %w", err)
	}
	if err := s.rdb.Set(ctx, key, payload, s.ttl).Err(); err != nil {
		return fmt.Errorf("store idempotency record: %w", err)
	}
	return nil
}

// Release drops the claim so the client can retry.
//
// It is called when the handler panicked or answered 5xx: the outcome is
// unknown or a server fault, and pinning that answer for 24 hours would leave
// the merchant unable to ever complete the payment.
func (s *Store) Release(ctx context.Context, key string) error {
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// Fingerprint hashes the request body so a reused key with different content
// can be detected.
//
// The body is canonicalised first (JSON re-encoded with sorted keys) so that
// two byte-different but semantically identical retries -- different key order,
// different whitespace, which HTTP clients and proxies do produce -- are not
// mistaken for a different request.
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(canonicalize(body))
	return hex.EncodeToString(sum[:])
}

func canonicalize(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		// Not JSON: hash the raw bytes.
		return body
	}
	out, err := json.Marshal(sortValue(v))
	if err != nil {
		return body
	}
	return out
}

// sortValue rebuilds maps as ordered structures. encoding/json already sorts
// map keys when marshalling, so recursing is enough to make the output stable.
func sortValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(t))
		for _, k := range keys {
			out[k] = sortValue(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sortValue(e)
		}
		return out
	default:
		return v
	}
}
