package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/observability"
	redisstore "github.com/blkst8/scorpion/internal/redis"
)

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
) {
	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()

	heartbeatTicker := time.NewTicker(cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	connTTL := cfg.ConnTTL

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeatTicker.C:
			sendHeartbeat(w, flusher, clientID, clientIP)

		case <-pollTicker.C:
			// Always refresh connection TTL, even if there are no events.
			if err := conns.Refresh(ctx, clientID, clientIP, connTTL); err != nil {
				observability.Logger.Error("failed to refresh conn TTL",
					"client_id", clientID, "ip", clientIP, "error", err)
			}

			// Batch drain events
			batch, err := events.Drain(ctx, clientID, cfg.BatchSize)
			if err != nil {
				observability.Logger.Error("failed to drain events",
					"client_id", clientID, "error", err)
				observability.EventDrainErrorsTotal.Inc()
				continue
			}

			observability.EventsDrainedPerTick.Observe(float64(len(batch)))

			for _, raw := range batch {
				var e EventPayload
				if err := json.Unmarshal([]byte(raw), &e); err != nil {
					observability.Logger.Warn("malformed event", "client_id", clientID, "raw", raw, "error", err)
					continue
				}

				eventType := e.Type
				if eventType == "" {
					eventType = "message"
				}

				fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.ID, eventType, string(e.Data))
				observability.EventsDeliveredTotal.WithLabelValues(eventType).Inc()
			}

			if len(batch) > 0 {
				flusher.Flush()
			}
		}
	}
}
