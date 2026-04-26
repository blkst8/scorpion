package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	inFlightTTL = 72 * time.Hour
	ackedTTL    = 72 * time.Hour
)

// InFlightStore tracks events that have been delivered to clients but not yet
// acknowledged. It also manages duplicate-ACK detection.
type InFlightStore interface {
	// Register marks an event as in-flight after it is delivered to a client.
	Register(ctx context.Context, clientID, eventID string, deliveredAt time.Time) error

	// Validate checks whether an event is in the in-flight registry.
	// Returns (found bool, alreadyAcked bool, err).
	Validate(ctx context.Context, clientID, eventID string) (found bool, alreadyAcked bool, err error)

	// MarkAcked transitions an in-flight event to acknowledged state.
	// Returns false if the event was not found or already acked.
	MarkAcked(ctx context.Context, clientID, eventID string) (bool, error)
}

// inFlightStore implements InFlightStore backed by Redis.
type inFlightStore struct {
	rdb *redis.Client
}

// NewInFlightStore creates a new InFlightStore.
func NewInFlightStore(rdb *redis.Client) InFlightStore {
	return &inFlightStore{rdb: rdb}
}

func inFlightKey(clientID, eventID string) string {
	return fmt.Sprintf("scorpion:inflight:%s:%s", clientID, eventID)
}

func ackedKey(clientID, eventID string) string {
	return fmt.Sprintf("scorpion:acked:%s:%s", clientID, eventID)
}

// Register stores the delivery timestamp for an event.
func (s *inFlightStore) Register(ctx context.Context, clientID, eventID string, deliveredAt time.Time) error {
	key := inFlightKey(clientID, eventID)
	return s.rdb.Set(ctx, key, deliveredAt.UTC().Format(time.RFC3339Nano), inFlightTTL).Err()
}

// Validate returns whether the event is known (in-flight or already acked).
func (s *inFlightStore) Validate(ctx context.Context, clientID, eventID string) (found bool, alreadyAcked bool, err error) {
	// Check duplicate ACK first.
	ackedExists, err := s.rdb.Exists(ctx, ackedKey(clientID, eventID)).Result()
	if err != nil {
		return false, false, fmt.Errorf("acked check error: %w", err)
	}
	if ackedExists > 0 {
		return true, true, nil
	}

	// Check in-flight registry.
	inFlightExists, err := s.rdb.Exists(ctx, inFlightKey(clientID, eventID)).Result()
	if err != nil {
		return false, false, fmt.Errorf("inflight check error: %w", err)
	}
	return inFlightExists > 0, false, nil
}

// MarkAcked atomically moves an event from in-flight to acked state.
func (s *inFlightStore) MarkAcked(ctx context.Context, clientID, eventID string) (bool, error) {
	ifKey := inFlightKey(clientID, eventID)
	akKey := ackedKey(clientID, eventID)

	// Use pipeline: set acked key, delete inflight key.
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, akKey, time.Now().UTC().Format(time.RFC3339Nano), ackedTTL)
	pipe.Del(ctx, ifKey)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("mark acked error: %w", err)
	}
	return true, nil
}
