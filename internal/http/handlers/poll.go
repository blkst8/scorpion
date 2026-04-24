package handlers

import (
	"net/http"
	"strconv"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"

	"github.com/blkst8/scorpion/internal/domain"
	"github.com/blkst8/scorpion/internal/http/middleware"
	applog "github.com/blkst8/scorpion/internal/log"
)

// V1Poll processes GET /v1/events/:client_id.
// It drains up to `batch_size` events from the client's Redis queue and
// returns them as a JSON array. This endpoint is the HTTP-polling equivalent
// of the SSE stream and is primarily used for performance benchmarking.
// Requires TokenMiddleware; the caller's JWT sub must equal :client_id.
//
// Query params:
//
//	batch_size  int  (default: cfg.BatchSize, max: cfg.BatchSize)
func (h *HTTPHandlers) V1Poll(ctx echo.Context) error {
	callerID, _ := ctx.Get(middleware.ClientID).(string)
	clientID := ctx.Param("client_id")

	if clientID == "" {
		return ctx.JSON(http.StatusBadRequest, errorResponse{Error: "bad_request", Message: "Missing client_id."})
	}

	if callerID != clientID {
		return ctx.JSON(http.StatusForbidden, errorResponse{Error: "forbidden", Message: "You may only poll your own event queue."})
	}

	batchSize := h.Cfg.SSE.BatchSize
	if raw := ctx.QueryParam("batch_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= h.Cfg.SSE.BatchSize {
			batchSize = n
		}
	}

	rawEvents, _, err := h.Events.Drain(ctx.Request().Context(), clientID, batchSize)
	if err != nil {
		h.Log.Error("poll drain failed", applog.FieldClientID, clientID, applog.FieldError, err)
		return ctx.JSON(http.StatusInternalServerError, errorResponse{Error: "internal_error", Message: "Failed to read events."})
	}

	payloads := make([]domain.EventPayload, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var p domain.EventPayload
		if err := sonic.Unmarshal([]byte(raw), &p); err != nil {
			h.Log.Warn("poll: skipping malformed event", applog.FieldClientID, clientID, applog.FieldError, err)
			continue
		}
		payloads = append(payloads, p)
	}

	return ctx.JSON(http.StatusOK, map[string]any{
		"events": payloads,
		"count":  len(payloads),
	})
}
