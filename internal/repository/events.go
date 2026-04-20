package redisstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// EventStore manages per-client event queues in Redis.
type EventStore struct {
	rdb *redis.Client
}

// NewEventStore creates a new EventStore.
func NewEventStore(rdb *redis.Client) *EventStore {
	return &EventStore{rdb: rdb}
}

// eventsKey returns the Redis list key for a client's event queue.
func eventsKey(clientID string) string {
	return fmt.Sprintf("scorpion:events:%s", clientID)
}

// Drain atomically fetches up to batchSize events and trims the list.
// Uses LRANGE + LTRIM in a pipeline for a single round-trip.
func (s *EventStore) Drain(ctx context.Context, clientID string, batchSize int) ([]string, error) {
	key := eventsKey(clientID)

	pipe := s.rdb.Pipeline()
	lrangeCmd := pipe.LRange(ctx, key, 0, int64(batchSize-1))
	pipe.LTrim(ctx, key, int64(batchSize), -1)

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("failed to drain events: %w", err)
	}

	return lrangeCmd.Val(), nil
}

// Push adds an event JSON string to the front of a client's queue (LPUSH).
func (s *EventStore) Push(ctx context.Context, clientID, eventJSON string) error {
	return s.rdb.LPush(ctx, eventsKey(clientID), eventJSON).Err()
}
