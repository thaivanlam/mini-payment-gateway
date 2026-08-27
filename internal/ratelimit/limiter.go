// Package ratelimit caps how fast one merchant may call the API.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Result describes the outcome of one rate limit check.
type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

// Limiter is a fixed-window counter in Redis.
//
// A fixed window can let through up to 2x the limit across a window boundary.
// That is an accepted trade for this project: it costs one INCR per request
// with no Lua script, and the burst is bounded. A sliding window log or a token
// bucket would be the next step if the burst mattered.
type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
}

// New builds a Limiter allowing limit requests per window.
func New(rdb *redis.Client, limit int, window time.Duration) *Limiter {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{rdb: rdb, limit: limit, window: window}
}

// Allow counts one request against subject's budget.
//
// INCR and EXPIRE are pipelined so the round trip is paid once. EXPIRE is set
// on every call rather than only the first: it is idempotent here because the
// TTL is anchored to the window length, and it removes the failure mode where a
// crash between INCR and EXPIRE leaves an immortal counter.
func (l *Limiter) Allow(ctx context.Context, subject string) (Result, error) {
	now := time.Now()
	bucket := now.UnixNano() / int64(l.window)
	key := fmt.Sprintf("rl:%s:%d", subject, bucket)

	pipe := l.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, l.window)
	if _, err := pipe.Exec(ctx); err != nil {
		return Result{}, fmt.Errorf("rate limit check: %w", err)
	}

	count := int(incr.Val())
	remaining := l.limit - count
	if remaining < 0 {
		remaining = 0
	}

	windowEnd := time.Unix(0, (bucket+1)*int64(l.window))
	return Result{
		Allowed:    count <= l.limit,
		Limit:      l.limit,
		Remaining:  remaining,
		RetryAfter: time.Until(windowEnd),
	}, nil
}
