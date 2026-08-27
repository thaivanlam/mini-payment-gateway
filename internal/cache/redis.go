// Package cache owns the Redis connection shared by the idempotency store and
// the rate limiter.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Connect parses a redis:// URL, opens a client and verifies it answers.
func Connect(ctx context.Context, url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opt.PoolSize = 20
	opt.MinIdleConns = 2
	opt.ReadTimeout = 2 * time.Second
	opt.WriteTimeout = 2 * time.Second

	client := redis.NewClient(opt)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
