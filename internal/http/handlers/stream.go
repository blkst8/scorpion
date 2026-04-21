package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/blkst8/scorpion/internal/http/middleware"
	"github.com/labstack/echo/v4"
	"github.com/realclientip/realclientip-go"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/config"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
	redisstore "github.com/blkst8/scorpion/internal/repository"
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
func (h *SSEHandler) Handle(ctx echo.Context) error {
	r := ctx.Request()
	w := ctx.Response().Writer

	flusher, ok := w.(http.Flusher)
	if !ok {
		return ctx.JSON(
			http.StatusInternalServerError,
			map[string]string{
				"error":   "internal_error",
				"message": "Streaming not supported.",
			},
		)
	}

	ticketStr := r.URL.Query().Get("ticket")
	if ticketStr == "" {
		return ctx.JSON(
			http.StatusForbidden,
			map[string]string{
				"error":   "access_denied",
				"message": "Ticket is invalid, expired, or IP mismatch.",
			},
		)
	}

	claims, err := auth.ValidateTicket(h.cfg.Auth, ticketStr)
	if err != nil {
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

		return ctx.JSON(
			http.StatusForbidden,
			map[string]string{
				"error":   "access_denied",
				"message": "Ticket is invalid, expired, or IP mismatch.",
			},
		)
	}

	clientID := claims.Subject
	ticketIP := claims.IP
	jti := claims.ID

	requestIP := ctx.Get(middleware.ClientIP).(string)
	if requestIP == "" {
		return ctx.JSON(
			http.StatusInternalServerError,
			map[string]string{
				"error":   "internal_error",
				"message": "Failed to determine client IP.",
			},
		)
	}

	if requestIP != ticketIP {
		h.metrics.AuthIPMismatchTotal.Inc()
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

		h.log.Warn("IP mismatch",
			applog.FieldClientID, clientID,
			applog.FieldTicketIP, ticketIP,
			applog.FieldRequestIP, requestIP,
		)

		return ctx.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	ok, err = h.tickets.ValidateAndConsume(r.Context(), clientID, requestIP, jti)
	if err != nil {
		h.log.Error("ticket validate error",
			applog.FieldClientID, clientID,
			applog.FieldError, err,
		)

		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Ticket validation failed.",
		})
	}
	if !ok {
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

		h.log.Warn("ticket not found or jti mismatch",
			applog.FieldClientID, clientID,
			applog.FieldIP, requestIP,
		)

		return ctx.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	registered, err := h.conns.Register(r.Context(), clientID, requestIP, h.cfg.SSE.ConnTTL)
	if err != nil {
		h.log.Error("connection register error",
			applog.FieldClientID, clientID,
			applog.FieldError, err,
		)

		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to register connection.",
		})
	}
	if !registered {
		h.metrics.DuplicateConnectionsTotal.Inc()
		h.metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

		h.log.Warn("duplicate connection",
			applog.FieldClientID, clientID,
			applog.FieldIP, requestIP,
		)

		return ctx.JSON(http.StatusTooManyRequests, map[string]string{
			"error":   "duplicate_connection",
			"message": "An active stream already exists for this user and IP.",
		})
	}

	h.metrics.ConnectionsTotal.WithLabelValues("ok").Inc()
	h.metrics.ActiveConnections.Inc()
	defer h.metrics.ActiveConnections.Dec()

	h.log.Info("stream started",
		applog.FieldClientID, clientID,
		applog.FieldIP, requestIP,
		applog.FieldJTI, jti,
	)

	defer func() {
		if err := h.conns.Delete(context.Background(), clientID, requestIP); err != nil {
			h.log.Warn("failed to delete connection on close",
				applog.FieldClientID, clientID,
				applog.FieldError, err,
			)
		}
		h.log.Info("stream closed",
			applog.FieldClientID, clientID,
			applog.FieldIP, requestIP,
		)
	}()

	ctx.Response().Header().Set("Content-Type", "text/event-stream")
	ctx.Response().Header().Set("Cache-Control", "no-cache")
	ctx.Response().Header().Set("Connection", "keep-alive")
	ctx.Response().Header().Set("X-Accel-Buffering", "no")
	ctx.Response().WriteHeader(http.StatusOK)

	stream.RunLoop(r.Context(), w, flusher, clientID, requestIP, h.cfg.SSE, h.conns, h.events, h.log, h.metrics)

	return nil
}
