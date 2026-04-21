package redisstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// EventStore manages per-client event queues in Redis.
type EventStore struct {
	rdb           *redis.Client
	maxQueueDepth int64
}

// NewEventStore creates a new EventStore.
func NewEventStore(rdb *redis.Client, maxQueueDepth int64) *EventStore {
	return &EventStore{rdb: rdb, maxQueueDepth: maxQueueDepth}
}

// eventsKey returns the Redis list key for a client's event queue.
func eventsKey(clientID string) string {
	return fmt.Sprintf("scorpion:events:%s", clientID)
}

// Push appends an event JSON string to the tail of a client's queue (RPUSH)
// and atomically caps the list at maxQueueDepth to prevent unbounded growth.
func (s *EventStore) Push(ctx context.Context, clientID, eventJSON string) error {
	key := eventsKey(clientID)
	pipe := s.rdb.Pipeline()
	pipe.RPush(ctx, key, eventJSON)
	pipe.LTrim(ctx, key, -s.maxQueueDepth, -1)
	_, err := pipe.Exec(ctx)
	return err
}

// Drain atomically fetches up to batchSize events from the head, trims the
// list, and also returns the remaining queue depth (via LLEN in the same
// pipeline) for metrics.
func (s *EventStore) Drain(ctx context.Context, clientID string, batchSize int) (events []string, remaining int64, err error) {
	key := eventsKey(clientID)

	pipe := s.rdb.Pipeline()
	lrangeCmd := pipe.LRange(ctx, key, 0, int64(batchSize-1))
	pipe.LTrim(ctx, key, int64(batchSize), -1)
	llenCmd := pipe.LLen(ctx, key)

	if _, execErr := pipe.Exec(ctx); execErr != nil && !errors.Is(execErr, redis.Nil) {
		return nil, 0, fmt.Errorf("failed to drain events: %w", execErr)
	}

	return lrangeCmd.Val(), llenCmd.Val(), nil
}
