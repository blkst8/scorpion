package auth

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/realclientip/realclientip-go"

	"github.com/blkst8/scorpion/internal/config"
)

// TokenMiddleware validates the real Bearer token and sets client_id/client_ip in context.
// Used for routes that require authentication beyond the ticket system.
func TokenMiddleware(cfg config.Auth, ipStrategy realclientip.Strategy, log *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authHeader := c.Request().Header.Get("Authorization")
			if len(authHeader) < 8 || authHeader[:7] != "Bearer " {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error":   "invalid_token",
					"message": "The provided token is invalid or expired.",
				})
			}

			rawToken := authHeader[7:]
			clientID, err := ValidateRealToken(cfg, rawToken)
			if err != nil {
				log.Warn("middleware token validation failed", "error", err.Error())
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error":   "invalid_token",
					"message": "The provided token is invalid or expired.",
				})
			}

			clientIP := ipStrategy.ClientIP(c.Request().Header, c.Request().RemoteAddr)
			if clientIP == "" {
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"error":   "internal_error",
					"message": "Failed to determine client IP.",
				})
			}

			c.Set("client_id", clientID)
			c.Set("client_ip", clientIP)
			return next(c)
		}
	}
}
