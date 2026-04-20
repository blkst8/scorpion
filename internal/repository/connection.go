package redisstore

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConnectionStore manages active SSE connection tracking in Redis.
type ConnectionStore struct {
	rdb        *redis.Client
	instanceID string
}

// NewConnectionStore creates a new ConnectionStore.
func NewConnectionStore(rdb *redis.Client, instanceID string) *ConnectionStore {
	return &ConnectionStore{rdb: rdb, instanceID: instanceID}
}

// connKey returns the Redis key for a connection.
func connKey(clientID, ip string) string {
	return fmt.Sprintf("scorpion:conn:%s:%s", clientID, ip)
}

// Register atomically registers a new connection using SetNX.
// Returns true if registration succeeded (no existing connection).
func (s *ConnectionStore) Register(ctx context.Context, clientID, ip string, ttl time.Duration) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, connKey(clientID, ip), s.instanceID, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("failed to register connection: %w", err)
	}
	return ok, nil
}

// Refresh extends the TTL on an existing connection key.
func (s *ConnectionStore) Refresh(ctx context.Context, clientID, ip string, ttl time.Duration) error {
	return s.rdb.Expire(ctx, connKey(clientID, ip), ttl).Err()
}

// Delete removes the connection key on disconnect.
func (s *ConnectionStore) Delete(ctx context.Context, clientID, ip string) error {
	return s.rdb.Del(ctx, connKey(clientID, ip)).Err()
}

// CleanupInstance scans and removes all connection keys belonging to this instance.
// Used during forced shutdown to avoid ghost connections.
func (s *ConnectionStore) CleanupInstance(ctx context.Context) {
	var cursor uint64
	for {
		keys, nextCursor, err := s.rdb.Scan(ctx, cursor, "scorpion:conn:*", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			val, err := s.rdb.Get(ctx, key).Result()
			if err == nil && val == s.instanceID {
				s.rdb.Del(ctx, key)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
}

// DeleteWithValue removes the connection key only if it matches this instance.
func (s *ConnectionStore) DeleteWithValue(ctx context.Context, clientID, ip string) error {
	key := connKey(clientID, ip)
	val, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		return err
	}
	if val == s.instanceID {
		return s.rdb.Del(ctx, key).Err()
	}
	return nil
}
