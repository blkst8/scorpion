package redisstore

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/atomic_ticket_validate.lua
var atomicTicketValidateScript string

var atomicTicketValidateLua = redis.NewScript(atomicTicketValidateScript)

// TicketStore manages pre-auth ticket operations in Redis.
type TicketStore struct {
	rdb *redis.Client
}

// NewTicketStore creates a new TicketStore.
func NewTicketStore(rdb *redis.Client) *TicketStore {
	return &TicketStore{rdb: rdb}
}

// ticketKey returns the Redis key for a ticket.
func ticketKey(clientID, ip string) string {
	return fmt.Sprintf("scorpion:ticket:%s:%s", clientID, ip)
}

// Store saves a ticket JTI in Redis with the given TTL.
func (s *TicketStore) Store(ctx context.Context, clientID, ip, jti string, ttl time.Duration) error {
	return s.rdb.Set(ctx, ticketKey(clientID, ip), jti, ttl).Err()
}

// ValidateAndConsume atomically validates the ticket JTI and deletes it.
// Returns true if successful, false if not found or mismatched.
func (s *TicketStore) ValidateAndConsume(ctx context.Context, clientID, ip, jti string) (bool, error) {
	key := ticketKey(clientID, ip)
	result, err := atomicTicketValidateLua.Run(ctx, s.rdb, []string{key}, jti).Int()
	if err != nil {
		return false, fmt.Errorf("ticket validate script error: %w", err)
	}
	return result == 1, nil
}
