package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

var startTime = time.Now()

// HealthHandler returns the /healthz echo handler.
func HealthHandler(rdb *redis.Client, activeConns func() float64) echo.HandlerFunc {
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

		return c.JSON(http.StatusOK, map[string]interface{}{
			"status":             "ok",
			"redis":              "connected",
			"active_connections": activeConns(),
			"uptime_seconds":     int64(time.Since(startTime).Seconds()),
		})
	}
}
