package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/domain"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/repository"
	"github.com/blkst8/scorpion/internal/telemetry"
)

// sseWritePool pools bytes.Buffers used to build SSE frames without allocating
// on every event.
var sseWritePool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// writeSSEEvent formats and writes one SSE event frame using a pooled buffer.
func writeSSEEvent(w io.Writer, id, eventType, data string) error {
	buf := sseWritePool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("id: ")
	buf.WriteString(id)
	buf.WriteByte('\n')
	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteByte('\n')
	buf.WriteString("data: ")
	buf.WriteString(data)
	buf.WriteString("\n\n")
	_, err := w.Write(buf.Bytes())
	sseWritePool.Put(buf)
	return err
}

// circuitBreaker is a minimal circuit breaker that opens after maxFails
// consecutive failures and resets after resetAfter.
type circuitBreaker struct {
	maxFails   uint32
	resetAfter time.Duration

	consecutiveFails atomic.Uint32
	openUntil        atomic.Int64 // unix nano; 0 = closed
}

func newCircuitBreaker(maxFails uint32, resetAfter time.Duration) *circuitBreaker {
	return &circuitBreaker{maxFails: maxFails, resetAfter: resetAfter}
}

func (cb *circuitBreaker) isOpen() bool {
	until := cb.openUntil.Load()
	if until == 0 {
		return false
	}
	if time.Now().UnixNano() > until {
		cb.openUntil.Store(0)
		cb.consecutiveFails.Store(0)
		return false
	}
	return true
}

func (cb *circuitBreaker) recordSuccess() {
	cb.consecutiveFails.Store(0)
	cb.openUntil.Store(0)
}

func (cb *circuitBreaker) recordFailure() {
	n := cb.consecutiveFails.Add(1)
	if n >= cb.maxFails {
		cb.openUntil.Store(time.Now().Add(cb.resetAfter).UnixNano())
	}
}

// RunLoop runs the SSE event loop: polling Redis, draining events, and sending
// heartbeats. Blocks until ctx is cancelled.
func RunLoop(
	ctx context.Context,
	w io.Writer,
	flusher http.Flusher,
	clientID, clientIP string,
	cfg config.SSE,
	conns repository.ConnectionStore,
	events repository.EventStore,
	inFlight repository.InFlightStore,
	log *slog.Logger,
	m *metrics.Metrics,
) {
	rc := http.NewResponseController(w.(http.ResponseWriter))
	cb := newCircuitBreaker(5, 10*time.Second)

	// Drain any queued events immediately on connect so the client doesn't
	// have to wait a full poll interval for backlogged events.
	drainAndFlush(ctx, w, rc, flusher, clientID, cfg, events, inFlight, cb, log, m)

	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()
	heartbeatTicker := time.NewTicker(cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Graceful drain: deliver any queued events before the connection closes.
			drainAndFlush(context.Background(), w, rc, flusher, clientID, cfg, events, inFlight, cb, log, m)
			return

		case <-heartbeatTicker.C:
			if err := sendHeartbeat(w, rc, flusher, clientID, clientIP, log, m); err != nil {
				log.Warn("heartbeat write failed — client gone",
					applog.FieldClientID, clientID,
					applog.FieldError, err,
				)
				return
			}

		case <-pollTicker.C:
			if err := conns.Refresh(ctx, clientID, clientIP, cfg.ConnTTL); err != nil {
				log.Error("failed to refresh conn TTL",
					applog.FieldClientID, clientID,
					applog.FieldIP, clientIP,
					applog.FieldError, err,
				)
			}
			drainAndFlush(ctx, w, rc, flusher, clientID, cfg, events, inFlight, cb, log, m)
		}
	}
}

// drainAndFlush reads a batch from Redis and writes SSE frames to the client.
func drainAndFlush(
	ctx context.Context,
	w io.Writer,
	rc *http.ResponseController,
	flusher http.Flusher,
	clientID string,
	cfg config.SSE,
	events repository.EventStore,
	inFlight repository.InFlightStore,
	cb *circuitBreaker,
	log *slog.Logger,
	m *metrics.Metrics,
) {
	_, span := telemetry.Global.Start(ctx, "event.drain",
		telemetry.StringAttr(applog.FieldClientID, clientID),
	)
	defer span.End()

	if cb.isOpen() {
		return
	}

	batch, remaining, err := events.Drain(ctx, clientID, cfg.BatchSize)
	if err != nil {
		log.Error("failed to drain events",
			applog.FieldClientID, clientID,
			applog.FieldError, err,
		)
		m.EventDrainErrorsTotal.Inc()
		cb.recordFailure()
		span.SetError(err)
		return
	}
	cb.recordSuccess()

	m.EventsDrainedPerTick.Observe(float64(len(batch)))
	m.QueueDepth.Set(float64(remaining))

	for _, raw := range batch {
		var e domain.EventPayload
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			log.Warn("malformed event",
				applog.FieldClientID, clientID,
				applog.FieldRaw, raw,
				applog.FieldError, err,
			)
			continue
		}

		eventType := e.Type
		if eventType == "" {
			eventType = "message"
		}

		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := writeSSEEvent(w, e.ID, eventType, string(e.Data)); err != nil {
			log.Warn("SSE write failed — dropping client",
				applog.FieldClientID, clientID,
				applog.FieldError, err,
			)
			_ = rc.SetWriteDeadline(time.Time{})
			return
		}

		// Register the delivered event in the in-flight registry so the
		// ACK handler can validate it.
		if inFlight != nil {
			if regErr := inFlight.Register(ctx, clientID, e.ID, time.Now()); regErr != nil {
				log.Warn("failed to register in-flight event",
					applog.FieldClientID, clientID,
					"event_id", e.ID,
					applog.FieldError, regErr,
				)
			}
		}

		m.EventsDeliveredTotal.WithLabelValues(eventType).Inc()
	}

	if len(batch) > 0 {
		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		flusher.Flush()
		_ = rc.SetWriteDeadline(time.Time{})
	}
}
