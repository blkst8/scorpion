package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// HealthHandler returns the /healthz echo handler.
func HealthHandler(rdb *redis.Client) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		defer cancel()

		if err := rdb.Ping(ctx).Err(); err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]interface{}{
				"status": "degraded",
				"redis":  "disconnected",
				"error":  err.Error(),
			})
		}

		return c.NoContent(http.StatusNoContent)
	}
}
