package handlers

import (
	"context"
	"net/http"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/http/middleware"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/stream"
	"github.com/labstack/echo/v4"
)

// V1SSEStreamEvents processes GET /v1/stream/events.
func (h *HTTPHandlers) V1SSEStreamEvents(ctx echo.Context) error {
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

	claims, err := auth.ValidateTicket(h.Cfg.Auth, ticketStr)
	if err != nil {
		h.Metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

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
		h.Metrics.AuthIPMismatchTotal.Inc()
		h.Metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()
		h.Log.Warn("IP mismatch",
			applog.FieldClientID, clientID,
			applog.FieldTicketIP, ticketIP,
			applog.FieldRequestIP, requestIP,
		)

		return ctx.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	ok, err = h.Tickets.ValidateAndConsume(r.Context(), clientID, requestIP, jti)
	if err != nil {
		h.Log.Error("ticket validate error",
			applog.FieldClientID, clientID,
			applog.FieldError, err,
		)

		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Ticket validation failed.",
		})
	}
	if !ok {
		h.Metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

		h.Log.Warn("ticket not found or jti mismatch",
			applog.FieldClientID, clientID,
			applog.FieldIP, requestIP,
		)

		return ctx.JSON(http.StatusForbidden, map[string]string{
			"error":   "access_denied",
			"message": "Ticket is invalid, expired, or IP mismatch.",
		})
	}

	registered, err := h.Conns.Register(r.Context(), clientID, requestIP, h.Cfg.SSE.ConnTTL)
	if err != nil {
		h.Log.Error("connection register error",
			applog.FieldClientID, clientID,
			applog.FieldError, err,
		)

		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to register connection.",
		})
	}
	if !registered {
		h.Metrics.DuplicateConnectionsTotal.Inc()
		h.Metrics.ConnectionsTotal.WithLabelValues("rejected").Inc()

		h.Log.Warn("duplicate connection",
			applog.FieldClientID, clientID,
			applog.FieldIP, requestIP,
		)

		return ctx.JSON(http.StatusTooManyRequests, map[string]string{
			"error":   "duplicate_connection",
			"message": "An active stream already exists for this user and IP.",
		})
	}

	h.Metrics.ConnectionsTotal.WithLabelValues("ok").Inc()
	h.Metrics.ActiveConnections.Inc()
	defer h.Metrics.ActiveConnections.Dec()

	h.Log.Info("stream started",
		applog.FieldClientID, clientID,
		applog.FieldIP, requestIP,
		applog.FieldJTI, jti,
	)

	defer func() {
		if err := h.Conns.Delete(context.Background(), clientID, requestIP); err != nil {
			h.Log.Warn("failed to delete connection on close",
				applog.FieldClientID, clientID,
				applog.FieldError, err,
			)
		}
		h.Log.Info("stream closed",
			applog.FieldClientID, clientID,
			applog.FieldIP, requestIP,
		)
	}()

	ctx.Response().Header().Set("Content-Type", "text/event-stream")
	ctx.Response().Header().Set("Cache-Control", "no-cache")
	ctx.Response().Header().Set("Connection", "keep-alive")
	ctx.Response().Header().Set("X-Accel-Buffering", "no")
	ctx.Response().WriteHeader(http.StatusOK)
	// Flush headers immediately so the client receives the 200 OK and the SSE
	// handshake completes before the first poll tick.  Without this, clients
	// using ResponseHeaderTimeout will time out waiting for headers that are
	// buffered inside Go's http.ResponseWriter until the first Write/Flush.
	flusher.Flush()

	stream.RunLoop(r.Context(), h.AppCTX, w, flusher, clientID, requestIP, h.Cfg.SSE, h.Conns, h.Events, h.InFlight, h.Log, h.Metrics)

	return nil
}
