// Package repository provides Redis client initialization for Scorpion.
package redisstore

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/blkst8/scorpion/internal/config"
)

// NewClient creates and validates a new Redis client from configuration.
func NewClient(cfg config.Redis) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Address,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to repository: %w", err)
	}

	return rdb, nil
}
