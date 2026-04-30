package ack

import (
	"context"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/nats-io/nats.go"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/domain"
)

// natsPublisher publishes AckRecords to a NATS JetStream stream.
// All records are published to a single static subject derived from config.
type natsPublisher struct {
	js         nats.JetStreamContext
	nc         *nats.Conn
	subject    string
	timeout    time.Duration
	maxRetries int
}

// NewNATSPublisher creates a Publisher backed by a NATS JetStream context.
// subject is derived from cfg.SubjectPrefix (static — no per-client routing).
func NewNATSPublisher(nc *nats.Conn, cfg config.NATS) (Publisher, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("nats jetstream context: %w", err)
	}

	// Ensure the stream exists; create it when absent.
	_, err = js.StreamInfo(cfg.StreamName)
	if err != nil {
		_, err = js.AddStream(&nats.StreamConfig{
			Name:       cfg.StreamName,
			Subjects:   []string{cfg.SubjectPrefix + ".>"},
			Duplicates: cfg.DuplicateWindow,
		})
		if err != nil {
			return nil, fmt.Errorf("nats create stream %q: %w", cfg.StreamName, err)
		}
	}

	timeout := time.Duration(cfg.PublishTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	subject := cfg.SubjectPrefix // static subject for all ACK records

	return &natsPublisher{
		js:         js,
		nc:         nc,
		subject:    subject,
		timeout:    timeout,
		maxRetries: maxRetries,
	}, nil
}

// Publish marshals the AckRecord with sonic and publishes it to the static subject.
// Retries up to maxRetries times on transient errors.
func (p *natsPublisher) Publish(_ context.Context, record domain.AckRecord) (PublishResult, error) {
	payload, err := sonic.Marshal(record)
	if err != nil {
		return PublishResult{}, fmt.Errorf("ack marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		ack, err := p.js.Publish(p.subject, payload, nats.AckWait(p.timeout))
		if err == nil {
			return PublishResult{Seq: ack.Sequence}, nil
		}
		lastErr = err
	}

	return PublishResult{}, fmt.Errorf("nats publish to %q failed after %d attempts: %w", p.subject, p.maxRetries+1, lastErr)
}

// Connected reports whether the underlying NATS connection is open.
func (p *natsPublisher) Connected() bool {
	return p.nc.IsConnected()
}

// Close drains and closes the NATS connection gracefully.
func (p *natsPublisher) Close() {
	_ = p.nc.Drain()
}
