package app

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/blkst8/scorpion/internal/config"
)

// WithNats connects to NATS using the provided configuration and returns the
// connection. Returns an error instead of panicking so the caller can decide
// whether to fall back to the log publisher.
func WithNats(log *slog.Logger, cfg config.NATS) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name(cfg.StreamName),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1), // reconnect forever
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("nats disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			log.Warn("nats connection closed")
		}),
	}

	if cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredentialsFile))
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", cfg.URL, err)
	}

	log.Info("nats connected", "url", nc.ConnectedUrl(), "stream", cfg.StreamName)
	return nc, nil
}
