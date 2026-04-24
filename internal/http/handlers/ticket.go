// Package handlers provides HTTP request handlers for Scorpion.
package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/http/middleware"
	"github.com/labstack/echo/v4"
)

type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Handle processes POST /v1/auth/ticket.
func (h *HTTPHandlers) V1AuthTicket(ctx echo.Context) error {
	clientIP := ctx.Get(middleware.ClientIP).(string)
	if clientIP == "" {
		h.Log.Error("failed to extract client IP")

		return ctx.JSON(
			http.StatusInternalServerError,
			errorResponse{
				Error:   "internal_error",
				Message: "Failed to determine client IP.",
			},
		)
	}

	rl, err := h.Limiter.Allow(ctx.Request().Context(), clientIP)
	if err != nil {
		h.Log.Error("rate limiter error", "ip", clientIP, "error", err)
	}

	ctx.Response().Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", rl.Limit))
	ctx.Response().Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rl.Remaining))
	ctx.Response().Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", rl.ResetAt.Unix()))

	if !rl.Allowed {
		h.Metrics.RateLimitExceededTotal.Inc()
		h.Metrics.TicketsRejectedTotal.WithLabelValues("rate_limited").Inc()

		h.Log.Warn(
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

	ticketStr, jti, err := auth.GenerateTicket(h.Cfg.Auth, clientID, clientIP)
	if err != nil {
		h.Log.Error(
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

	err = h.Tickets.Store(ctx.Request().Context(), clientID, clientIP, jti, h.Cfg.Auth.TicketTTL)
	if err != nil {
		h.Log.Error(
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

	h.Metrics.TicketsIssuedTotal.Inc()

	h.Log.Info(
		"ticket issued",
		"client_id", clientID,
		"ip", clientIP,
		"expires_at", time.Now().Add(h.Cfg.Auth.TicketTTL),
	)

	return ctx.JSON(http.StatusOK, ticketResponse{
		Ticket:    ticketStr,
		ExpiresIn: int(h.Cfg.Auth.TicketTTL.Seconds()),
	})
}
