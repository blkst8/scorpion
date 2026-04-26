package ack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/blkst8/scorpion/internal/domain"
)

// logPublisher is a Publisher implementation that logs ACK records instead of
// publishing to NATS. Used when NATS is not configured or for testing.
type logPublisher struct {
	log *slog.Logger
}

// NewLogPublisher returns a Publisher that writes ACK records to the logger.
// This is a no-op substitute used when no NATS connection is configured.
func NewLogPublisher(log *slog.Logger) Publisher {
	return &logPublisher{log: log}
}

func (p *logPublisher) Publish(_ context.Context, record domain.AckRecord) (PublishResult, error) {
	b, _ := json.Marshal(record)
	// TODO Nima: fix this section
	p.log.Info("ack record (nats disabled)", "record", string(b))
	return PublishResult{Seq: 0}, nil
}

func (p *logPublisher) Connected() bool { return true }
func (p *logPublisher) Close()          {}

// retryPolicy holds the retry schedule for NATS publish failures.
var retryDelays = []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond}

// withRetry executes fn up to len(retryDelays) times with the defined backoff.
func withRetry(ctx context.Context, fn func() (PublishResult, error)) (PublishResult, error) {
	var (
		res PublishResult
		err error
	)
	for _, delay := range retryDelays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return PublishResult{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		res, err = fn()
		if err == nil {
			return res, nil
		}
	}
	return PublishResult{}, fmt.Errorf("all publish retries exhausted: %w", err)
}
