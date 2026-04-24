package repository

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/delete_if_owner.lua
var deleteIfOwnerScript string

var deleteIfOwnerLua = redis.NewScript(deleteIfOwnerScript)

type ConnectionStore interface {
	Register(ctx context.Context, clientID, ip string, ttl time.Duration) (bool, error)
	Refresh(ctx context.Context, clientID, ip string, ttl time.Duration) error
	Delete(ctx context.Context, clientID, ip string) error
	DeleteWithValue(ctx context.Context, clientID, ip string) error
	CleanupInstance(ctx context.Context)
}

// connectionStore manages active SSE connection tracking in Redis.
type connectionStore struct {
	rdb        *redis.Client
	instanceID string
	log        *slog.Logger
}

// NewConnectionStore creates a new ConnectionStore.
func NewConnectionStore(rdb *redis.Client, instanceID string, log *slog.Logger) ConnectionStore {
	return &connectionStore{rdb: rdb, instanceID: instanceID, log: log}
}

// connKey returns the Redis key for a connection.
func connKey(clientID, ip string) string {
	return fmt.Sprintf("scorpion:conn:%s:%s", clientID, ip)
}

// Register atomically registers a new connection using SetNX.
// Returns true if registration succeeded (no existing connection).
func (s *connectionStore) Register(ctx context.Context, clientID, ip string, ttl time.Duration) (bool, error) {
	result, err := s.rdb.SetArgs(ctx, connKey(clientID, ip), s.instanceID, redis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, fmt.Errorf("failed to register connection: %w", err)
	}
	return result == "OK", nil
}

// Refresh extends the TTL on an existing connection key.
func (s *connectionStore) Refresh(ctx context.Context, clientID, ip string, ttl time.Duration) error {
	return s.rdb.Expire(ctx, connKey(clientID, ip), ttl).Err()
}

// Delete removes the connection key on disconnect.
func (s *connectionStore) Delete(ctx context.Context, clientID, ip string) error {
	return s.rdb.Del(ctx, connKey(clientID, ip)).Err()
}

// DeleteWithValue atomically removes the connection key only if its stored
// value matches this instance ID, preventing cross-instance deletions.
func (s *connectionStore) DeleteWithValue(ctx context.Context, clientID, ip string) error {
	key := connKey(clientID, ip)
	_, err := deleteIfOwnerLua.Run(ctx, s.rdb, []string{key}, s.instanceID).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("delete_if_owner script error: %w", err)
	}
	return nil
}

// CleanupInstance scans and removes all connection keys belonging to this instance.
// Used during forced shutdown to avoid ghost connections.
func (s *connectionStore) CleanupInstance(ctx context.Context) {
	var cursor uint64
	for {
		keys, nextCursor, err := s.rdb.Scan(ctx, cursor, "scorpion:conn:*", 100).Result()
		if err != nil {
			s.log.Error("cleanup scan error", "error", err)
			break
		}
		for _, key := range keys {
			val, err := s.rdb.Get(ctx, key).Result()
			if err != nil {
				continue
			}
			if val == s.instanceID {
				if err := s.rdb.Del(ctx, key).Err(); err != nil {
					s.log.Error("cleanup del error", "key", key, "error", err)
				}
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
}
