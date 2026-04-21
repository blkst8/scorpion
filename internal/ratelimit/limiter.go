// Package ratelimit provides a per-IP sliding window rate limiter backed by Redis.
package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/blkst8/scorpion/internal/config"
)

//go:embed scripts/atomic_ratelimit.lua

var atomicRateLimitScript string

var atomicRateLimitLua = redis.NewScript(atomicRateLimitScript)

// Limiter is a Redis-backed per-IP rate limiter using an atomic Lua script.
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

const windowSeconds = 60

// Allow checks whether the given IP is within the rate limit.
// Uses an atomic Lua script: INCR + EXPIRE-on-first-hit in one round-trip.
func (l *Limiter) Allow(ctx context.Context, ip string) (Result, error) {
	key := fmt.Sprintf("scorpion:ratelimit:%s", ip)
	limit := l.cfg.TicketRPM + l.cfg.TicketBurst

	val, err := atomicRateLimitLua.Run(ctx, l.rdb, []string{key}, windowSeconds).Int()
	if err != nil {
		return Result{}, fmt.Errorf("rate limit script error: %w", err)
	}

	count := val
	remaining := limit - count
	if remaining < 0 {
		remaining = 0
	}

	// ResetAt is approximated: accurate enough for the X-RateLimit-Reset header.
	resetAt := time.Now().Add(windowSeconds * time.Second)

	return Result{
		Allowed:   count <= limit,
		Count:     count,
		Limit:     l.cfg.TicketRPM,
		Remaining: remaining,
		ResetAt:   resetAt,
	}, nil
}
