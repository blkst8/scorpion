// Package ack provides the NATS JetStream publisher interface and implementations
// for the Scorpion ACK handler service.
package ack

import (
	"context"

	"github.com/blkst8/scorpion/internal/domain"
)

// PublishResult holds the outcome of a NATS publish operation.
type PublishResult struct {
	// Seq is the JetStream sequence number assigned to the published message.
	Seq uint64
}

// Publisher defines the contract for publishing ACK records to NATS JetStream.
// This interface decouples the ACK handler from the concrete NATS client,
// enabling testing and future transport swaps.
type Publisher interface {
	// Publish publishes an AckRecord to NATS JetStream.
	// Returns the publish result or an error if all retries fail.
	Publish(ctx context.Context, record domain.AckRecord) (PublishResult, error)

	// Connected reports whether the publisher has an active NATS connection.
	Connected() bool

	// Close gracefully closes the NATS connection.
	Close()
}
