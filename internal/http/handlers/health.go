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
