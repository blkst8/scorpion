// Package handlers provides HTTP request handlers for Scorpion.
package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/blkst8/scorpion/internal/http/middleware"
	"github.com/labstack/echo/v4"
	"github.com/realclientip/realclientip-go"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/ratelimit"
	redisstore "github.com/blkst8/scorpion/internal/redis"
)

type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// TicketHandler handles POST /v1/auth/ticket.
type TicketHandler struct {
	cfg        config.Config
	tickets    *redisstore.TicketStore
	limiter    *ratelimit.Limiter
	ipStrategy realclientip.Strategy
	log        *slog.Logger
	metrics    *metrics.Metrics
}

// NewTicketHandler creates a new TicketHandler with all dependencies injected.
func NewTicketHandler(
	cfg config.Config,
	tickets *redisstore.TicketStore,
	limiter *ratelimit.Limiter,
	ipStrategy realclientip.Strategy,
	log *slog.Logger,
	m *metrics.Metrics,
) *TicketHandler {
	return &TicketHandler{
		cfg:        cfg,
		tickets:    tickets,
		limiter:    limiter,
		ipStrategy: ipStrategy,
		log:        log,
		metrics:    m,
	}
}

// Handle processes POST /v1/auth/ticket.
func (h *TicketHandler) Handle(ctx echo.Context) error {
	clientIP := ctx.Get(middleware.ClientIP).(string)
	if clientIP == "" {
		h.log.Error("failed to extract client IP")

		return ctx.JSON(
			http.StatusInternalServerError,
			errorResponse{
				Error:   "internal_error",
				Message: "Failed to determine client IP.",
			},
		)
	}

	rl, err := h.limiter.Allow(ctx.Request().Context(), clientIP)
	if err != nil {
		h.log.Error("rate limiter error", "ip", clientIP, "error", err)
	}

	ctx.Response().Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", rl.Limit))
	ctx.Response().Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rl.Remaining))
	ctx.Response().Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", rl.ResetAt.Unix()))

	if !rl.Allowed {
		h.metrics.RateLimitExceededTotal.Inc()
		h.metrics.TicketsRejectedTotal.WithLabelValues("rate_limited").Inc()

		h.log.Warn(
			"rate limit exceeded",
			"ip", clientIP,
			"count", rl.Count,
			"limit", rl.Limit,
			"reset_at", rl.ResetAt,
		)

		return ctx.JSON(
			http.StatusTooManyRequests,
			errorResponse{
				Error:   "rate_limit_exceeded",
				Message: "Too many ticket requests. Try again later.",
			},
		)
	}

	clientID := ctx.Get(middleware.ClientID).(string)

	ticketStr, jti, err := auth.GenerateTicket(h.cfg.Auth, clientID, clientIP)
	if err != nil {
		h.log.Error(
			"failed to generate ticket",
			"client_id", clientID,
			"ip", clientIP,
			"error", err,
		)

		return ctx.JSON(
			http.StatusInternalServerError,
			errorResponse{
				Error:   "internal_error",
				Message: "Failed to generate ticket.",
			},
		)
	}

	err = h.tickets.Store(ctx.Request().Context(), clientID, clientIP, jti, h.cfg.Auth.TicketTTL)
	if err != nil {
		h.log.Error(
			"failed to store ticket",
			"client_id", clientID,
			"ip", clientIP,
			"error", err,
		)

		return ctx.JSON(
			http.StatusInternalServerError,
			errorResponse{
				Error:   "internal_error",
				Message: "Failed to store ticket.",
			},
		)
	}

	h.metrics.TicketsIssuedTotal.Inc()

	h.log.Info(
		"ticket issued",
		"client_id", clientID,
		"ip", clientIP,
		"expires_at", time.Now().Add(h.cfg.Auth.TicketTTL),
	)

	return ctx.JSON(http.StatusOK, ticketResponse{
		Ticket:    ticketStr,
		ExpiresIn: int(h.cfg.Auth.TicketTTL.Seconds()),
	})
}
