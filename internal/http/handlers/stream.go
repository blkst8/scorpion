package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/realclientip/realclientip-go"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/metrics"
	redisstore "github.com/blkst8/scorpion/internal/redis"
	"github.com/blkst8/scorpion/internal/stream"
)

// SSEHandler handles GET /v1/stream/events.
type SSEHandler struct {
	cfg        config.Config
	tickets    *redisstore.TicketStore
	conns      *redisstore.ConnectionStore
	events     *redisstore.EventStore
	ipStrategy realclientip.Strategy
	log        *slog.Logger
	metrics    *metrics.Metrics
}

// NewSSEHandler creates a new SSEHandler with all dependencies injected.
func NewSSEHandler(
	cfg config.Config,
	tickets *redisstore.TicketStore,
	conns *redisstore.ConnectionStore,
	events *redisstore.EventStore,
	ipStrategy realclientip.Strategy,
	log *slog.Logger,
	m *metrics.Metrics,
) *SSEHandler {
	return &SSEHandler{
		cfg:        cfg,
		tickets:    tickets,
		conns:      conns,
		events:     events,
		ipStrategy: ipStrategy,
		log:        log,
		metrics:    m,
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

	ticketStr := r.URL.Query().Get("ticket")
	if ticketStr == "" {
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	claims, err := auth.ValidateTicket(h.cfg.Auth, ticketStr)
	if err != nil {
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	clientID := claims.Subject
	ticketIP := claims.IP
	jti := claims.ID

	requestIP := h.ipStrategy.ClientIP(r.Header, r.RemoteAddr)
	if requestIP == "" {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to determine client IP.",
		})
	}

	if requestIP != ticketIP {
		h.metrics.AuthIPMismatchTotal.Inc()
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()
		h.log.Warn("IP mismatch",
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

	ok, err = h.tickets.ValidateAndConsume(ctx, clientID, requestIP, jti)
	if err != nil {
		h.log.Error("ticket validate error", "client_id", clientID, "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Ticket validation failed.",
		})
	}
	if !ok {
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()
		h.log.Warn("ticket not found or jti mismatch", "client_id", clientID, "ip", requestIP)
		return c.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	registered, err := h.conns.Register(ctx, clientID, requestIP, h.cfg.SSE.ConnTTL)
	if err != nil {
		h.log.Error("connection register error", "client_id", clientID, "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to register connection.",
		})
	}
	if !registered {
		h.metrics.DuplicateConnectionsTotal.Inc()
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()
		h.log.Warn("duplicate connection", "client_id", clientID, "ip", requestIP)
		return c.JSON(http.StatusTooManyRequests, map[string]string{
			"error":   "duplicate_connection",
			"message": "An active stream already exists for this user and IP.",
		})
	}

	h.metrics.ConnectionsTotal.WithLabelValues("ok").Inc()
	h.metrics.ActiveConnections.Inc()
	defer h.metrics.ActiveConnections.Dec()

	h.log.Info("stream started", "client_id", clientID, "ip", requestIP, "jti", jti)

	defer func() {
		h.conns.Delete(context.Background(), clientID, requestIP)
		h.log.Info("stream closed", "client_id", clientID, "ip", requestIP)
	}()

	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no")
	c.Response().WriteHeader(http.StatusOK)

	stream.RunLoop(ctx, w, flusher, clientID, requestIP, h.cfg.SSE, h.conns, h.events, h.log, h.metrics)
	return nil
}
