package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// Healthz returns the /healthz echo handler.
func (h *HTTPHandlers) Healthz(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
	defer cancel()

	if err := h.RDB.Ping(ctx).Err(); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"status":     "degraded",
			"repository": "disconnected",
			"error":      err.Error(),
		})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz returns 200 only when all dependencies are ready.
// It checks Redis connectivity and NATS publisher connection.
func (h *HTTPHandlers) Readyz(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
	defer cancel()

	status := map[string]string{
		"redis": "ok",
		"nats":  "ok",
	}
	ready := true

	if err := h.RDB.Ping(ctx).Err(); err != nil {
		status["redis"] = "disconnected"
		ready = false
	}

	if h.AckPublisher != nil && !h.AckPublisher.Connected() {
		status["nats"] = "disconnected"
		ready = false
	}

	if !ready {
		return c.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"status": "not_ready",
			"checks": status,
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"status": "ready",
		"checks": status,
	})
}
