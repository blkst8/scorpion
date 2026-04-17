package auth

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/observability"
	"github.com/blkst8/scorpion/internal/ratelimit"
	redisstore "github.com/blkst8/scorpion/internal/redis"
	"github.com/realclientip/realclientip-go"
)

// TicketRequest is the response body for POST /v1/auth/ticket.
type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

type errorResponse struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// TicketHandler handles POST /v1/auth/ticket.
type TicketHandler struct {
	cfg        config.Config
	tickets    *redisstore.TicketStore
	limiter    *ratelimit.Limiter
	ipStrategy realclientip.Strategy
}

// NewTicketHandler creates a new TicketHandler.
func NewTicketHandler(
	cfg config.Config,
	tickets *redisstore.TicketStore,
	limiter *ratelimit.Limiter,
	ipStrategy realclientip.Strategy,
) *TicketHandler {
	return &TicketHandler{
		cfg:        cfg,
		tickets:    tickets,
		limiter:    limiter,
		ipStrategy: ipStrategy,
	}
}

// Handle processes POST /v1/auth/ticket.
func (h *TicketHandler) Handle(c echo.Context) error {
	ctx := c.Request().Context()

	// Extract client IP
	clientIP := h.ipStrategy.ClientIP(c.Request().Header, c.Request().RemoteAddr)
	if clientIP == "" {
		observability.Logger.Error("failed to extract client IP")
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error:   "internal_error",
			Message: "Failed to determine client IP.",
		})
	}

	// Rate limit check
	rl, err := h.limiter.Allow(ctx, clientIP)
	if err != nil {
		observability.Logger.Error("rate limiter error", "ip", clientIP, "error", err)
	}

	// Set rate limit headers
	c.Response().Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", rl.Limit))
	c.Response().Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rl.Remaining))
	c.Response().Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", rl.ResetAt.Unix()))

	if !rl.Allowed {
		observability.RateLimitExceededTotal.Inc()
		observability.TicketsRejectedTotal.WithLabelValues("rate_limited").Inc()
		observability.Logger.Warn("rate limit exceeded", "ip", clientIP, "count", rl.Count, "limit", rl.Limit)

		retryAfter := int(time.Until(rl.ResetAt).Seconds()) + 1
		c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		return c.JSON(http.StatusTooManyRequests, errorResponse{
			Error:      "rate_limit_exceeded",
			Message:    "Too many ticket requests. Try again later.",
			RetryAfter: retryAfter,
		})
	}

	// Extract and validate Bearer token
	authHeader := c.Request().Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		observability.TicketsRejectedTotal.WithLabelValues("invalid").Inc()
		return c.JSON(http.StatusUnauthorized, errorResponse{
			Error:   "invalid_token",
			Message: "The provided token is invalid or expired.",
		})
	}
	rawToken := strings.TrimPrefix(authHeader, "Bearer ")

	clientID, err := ValidateRealToken(h.cfg.Auth, rawToken)
	if err != nil {
		observability.TicketsRejectedTotal.WithLabelValues("invalid").Inc()
		observability.Logger.Warn("token validation failed", "ip", clientIP, "reason", err.Error())
		return c.JSON(http.StatusUnauthorized, errorResponse{
			Error:   "invalid_token",
			Message: "The provided token is invalid or expired.",
		})
	}

	// Generate ticket
	ticketStr, jti, err := GenerateTicket(h.cfg.Auth, clientID, clientIP)
	if err != nil {
		observability.Logger.Error("failed to generate ticket", "client_id", clientID, "ip", clientIP, "error", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error:   "internal_error",
			Message: "Failed to generate ticket.",
		})
	}

	// Store ticket JTI in Redis
	if err := h.tickets.Store(ctx, clientID, clientIP, jti, h.cfg.Auth.TicketTTL); err != nil {
		observability.Logger.Error("failed to store ticket", "client_id", clientID, "ip", clientIP, "error", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error:   "internal_error",
			Message: "Failed to store ticket.",
		})
	}

	observability.TicketsIssuedTotal.Inc()
	observability.Logger.Info("ticket issued",
		"client_id", clientID,
		"ip", clientIP,
		"expires_at", time.Now().Add(h.cfg.Auth.TicketTTL),
	)

	return c.JSON(http.StatusOK, ticketResponse{
		Ticket:    ticketStr,
		ExpiresIn: int(h.cfg.Auth.TicketTTL.Seconds()),
	})
}
