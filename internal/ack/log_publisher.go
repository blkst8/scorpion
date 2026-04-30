package ack

import (
	"context"
	"encoding/json"
	"log/slog"

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
	p.log.Info("ack record (nats disabled)", "record", string(b))
	return PublishResult{Seq: 0}, nil
}

func (p *logPublisher) Connected() bool { return true }
func (p *logPublisher) Close()          {}
