// Package stream provides the SSE event streaming handler for Scorpion.
package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/observability"
	redisstore "github.com/blkst8/scorpion/internal/redis"
	"github.com/realclientip/realclientip-go"
)

// EventPayload represents a single SSE event from the queue.
type EventPayload struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// SSEHandler handles GET /v1/stream/events.
type SSEHandler struct {
	cfg        config.Config
	tickets    *redisstore.TicketStore
	conns      *redisstore.ConnectionStore
	events     *redisstore.EventStore
	ipStrategy realclientip.Strategy
}

// NewSSEHandler creates a new SSEHandler.
func NewSSEHandler(
	cfg config.Config,
	tickets *redisstore.TicketStore,
	conns *redisstore.ConnectionStore,
	events *redisstore.EventStore,
	ipStrategy realclientip.Strategy,
) *SSEHandler {
	return &SSEHandler{
		cfg:        cfg,
		tickets:    tickets,
		conns:      conns,
		events:     events,
		ipStrategy: ipStrategy,
	}
}

// Handle processes GET /v1/stream/events.
func (h *SSEHandler) Handle(c echo.Context) error {
	r := c.Request()
	w := c.Response().Writer

	flusher, ok := w.(http.Flusher)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Streaming not supported.",
		})
	}

	// --- Connection Phase ---

	ticketStr := r.URL.Query().Get("ticket")
	if ticketStr == "" {
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	// Validate ticket JWT
	claims, err := auth.ValidateTicket(h.cfg.Auth, ticketStr)
	if err != nil {
		observability.ConnectionsTotal.WithLabelValues("rejected").Inc()
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	clientID := claims.Subject
	ticketIP := claims.IP
	jti := claims.ID

	// Extract real client IP and compare to ticket claim
	requestIP := h.ipStrategy.ClientIP(r.Header, r.RemoteAddr)
	if requestIP == "" {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to determine client IP.",
		})
	}

	if requestIP != ticketIP {
		observability.AuthIPMismatchTotal.Inc()
		observability.ConnectionsTotal.WithLabelValues("rejected").Inc()
		observability.Logger.Warn("IP mismatch",
			"client_id", clientID,
			"ticket_ip", ticketIP,
			"request_ip", requestIP,
		)
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	ctx := r.Context()

	// Atomically validate and consume ticket (single-use enforcement)
	ok, err = h.tickets.ValidateAndConsume(ctx, clientID, requestIP, jti)
	if err != nil {
		observability.Logger.Error("ticket validate error", "client_id", clientID, "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Ticket validation failed.",
		})
	}
	if !ok {
		observability.ConnectionsTotal.WithLabelValues("rejected").Inc()
		observability.Logger.Warn("ticket not found or jti mismatch", "client_id", clientID, "ip", requestIP)
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	// Atomically register connection (duplicate check)
	connTTL := h.cfg.SSE.ConnTTL
	registered, err := h.conns.Register(ctx, clientID, requestIP, connTTL)
	if err != nil {
		observability.Logger.Error("connection register error", "client_id", clientID, "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to register connection.",
		})
	}
	if !registered {
		observability.DuplicateConnectionsTotal.Inc()
		observability.ConnectionsTotal.WithLabelValues("rejected").Inc()
		observability.Logger.Warn("duplicate connection", "client_id", clientID, "ip", requestIP)
		return c.JSON(http.StatusTooManyRequests, map[string]string{
			"error":   "duplicate_connection",
			"message": "An active stream already exists for this user and IP.",
		})
	}

	// --- Streaming Phase ---
	observability.ConnectionsTotal.WithLabelValues("ok").Inc()
	observability.ActiveConnections.Inc()
	defer observability.ActiveConnections.Dec()

	observability.Logger.Info("stream started",
		"client_id", clientID,
		"ip", requestIP,
		"jti", jti,
	)

	defer func() {
		h.conns.Delete(context.Background(), clientID, requestIP)
		observability.Logger.Info("stream closed",
			"client_id", clientID,
			"ip", requestIP,
		)
	}()

	// Set SSE headers
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no")
	c.Response().WriteHeader(http.StatusOK)

	RunLoop(ctx, w, flusher, clientID, requestIP, h.cfg.SSE, h.conns, h.events)
	return nil
}
