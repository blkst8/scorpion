package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/metrics"
	redisstore "github.com/blkst8/scorpion/internal/redis"
)

// eventPayload represents a single SSE event from the Redis queue.
type eventPayload struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// RunLoop runs the SSE event loop: polling Redis, draining events, and sending heartbeats.
// It blocks until the context is cancelled (client disconnects or server shuts down).
func RunLoop(
	ctx context.Context,
	w io.Writer,
	flusher http.Flusher,
	clientID, clientIP string,
	cfg config.SSE,
	conns *redisstore.ConnectionStore,
	events *redisstore.EventStore,
	log *slog.Logger,
	m *metrics.Metrics,
) {
	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()

	heartbeatTicker := time.NewTicker(cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	connTTL := cfg.ConnTTL

	for {
		select {
		case <-ctx.Done():
			// Client close the connection
			return

		case <-heartbeatTicker.C:
			// SSE connections traveling through proxies, load balancers, CDNs,
			// and cloud infrastructure can be silently terminated if idle.
			// Scorpion MUST send periodic **SSE comment lines** as heartbeats.
			sendHeartbeat(w, flusher, clientID, clientIP, log, m)

		case <-pollTicker.C:
			// Read batch events and send it to client
			if err := conns.Refresh(ctx, clientID, clientIP, connTTL); err != nil {
				log.Error(
					"failed to refresh conn TTL",
					"client_id", clientID,
					"ip", clientIP,
					"error", err,
				)
			}

			batch, err := events.Drain(ctx, clientID, cfg.BatchSize)
			if err != nil {
				log.Error(
					"failed to drain events",
					"client_id", clientID,
					"error", err,
				)

				m.EventDrainErrorsTotal.Inc()

				continue
			}

			m.EventsDrainedPerTick.Observe(float64(len(batch)))

			for _, raw := range batch {
				var e eventPayload
				if err := json.Unmarshal([]byte(raw), &e); err != nil {
					log.Warn(
						"malformed event",
						"client_id", clientID,
						"raw", raw,
						"error", err,
					)

					continue
				}

				eventType := e.Type
				if eventType == "" {
					eventType = "message"
				}

				fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.ID, eventType, string(e.Data))

				m.EventsDeliveredTotal.WithLabelValues(eventType).Inc()
			}

			if len(batch) > 0 {
				flusher.Flush()
			}
		}
	}
}
