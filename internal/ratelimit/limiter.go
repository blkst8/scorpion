// Package ratelimit provides a per-IP sliding window rate limiter backed by Redis.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/blkst8/scorpion/internal/config"
)

// Limiter is a Redis-backed per-IP sliding window rate limiter.
type Limiter struct {
	rdb *redis.Client
	cfg config.RateLimit
}

// NewLimiter creates a new Limiter.
func NewLimiter(rdb *redis.Client, cfg config.RateLimit) *Limiter {
	return &Limiter{rdb: rdb, cfg: cfg}
}

// Result holds the outcome of a rate limit check.
type Result struct {
	Allowed   bool
	Count     int
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// Allow checks whether the given IP is within the rate limit.
// Uses Redis INCR + EXPIRE in a pipeline for an atomic sliding window.
func (l *Limiter) Allow(ctx context.Context, ip string) (Result, error) {
	key := fmt.Sprintf("scorpion:ratelimit:%s", ip)
	limit := l.cfg.TicketRPM + l.cfg.TicketBurst

	pipe := l.rdb.Pipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 60*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return Result{}, fmt.Errorf("rate limit pipeline error: %w", err)
	}

	count := int(incrCmd.Val())
	remaining := limit - count
	if remaining < 0 {
		remaining = 0
	}

	// Approximate reset time
	ttl, _ := l.rdb.TTL(ctx, key).Result()
	resetAt := time.Now().Add(ttl)

	return Result{
		Allowed:   count <= limit,
		Count:     count,
		Limit:     l.cfg.TicketRPM,
		Remaining: remaining,
		ResetAt:   resetAt,
	}, nil
}
