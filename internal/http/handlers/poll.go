package handlers

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/domain"
	"github.com/blkst8/scorpion/internal/http/middleware"
	applog "github.com/blkst8/scorpion/internal/log"
	redisstore "github.com/blkst8/scorpion/internal/repository"
)

// PollHandler handles GET /v1/events/:client_id.
// It drains up to `batch_size` events from the client's Redis queue and
// returns them as a JSON array. This endpoint is the HTTP-polling equivalent
// of the SSE stream and is primarily used for performance benchmarking.
type PollHandler struct {
	events *redisstore.EventStore
	cfg    config.SSE
	log    *slog.Logger
}

// NewPollHandler creates a new PollHandler.
func NewPollHandler(events *redisstore.EventStore, cfg config.SSE, log *slog.Logger) *PollHandler {
	return &PollHandler{events: events, cfg: cfg, log: log}
}

// Handle processes GET /v1/events/:client_id.
// Requires TokenMiddleware; the caller's JWT sub must equal :client_id.
//
// Query params:
//
//	batch_size  int  (default: cfg.BatchSize, max: cfg.BatchSize)
func (h *PollHandler) Handle(ctx echo.Context) error {
	callerID, _ := ctx.Get(middleware.ClientID).(string)
	clientID := ctx.Param("client_id")

	if clientID == "" {
		return ctx.JSON(http.StatusBadRequest, errorResponse{Error: "bad_request", Message: "Missing client_id."})
	}

	if callerID != clientID {
		return ctx.JSON(http.StatusForbidden, errorResponse{Error: "forbidden", Message: "You may only poll your own event queue."})
	}

	batchSize := h.cfg.BatchSize
	if raw := ctx.QueryParam("batch_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= h.cfg.BatchSize {
			batchSize = n
		}
	}

	rawEvents, _, err := h.events.Drain(ctx.Request().Context(), clientID, batchSize)
	if err != nil {
		h.log.Error("poll drain failed", applog.FieldClientID, clientID, applog.FieldError, err)
		return ctx.JSON(http.StatusInternalServerError, errorResponse{Error: "internal_error", Message: "Failed to read events."})
	}

	payloads := make([]domain.EventPayload, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var p domain.EventPayload
		if err := sonic.Unmarshal([]byte(raw), &p); err != nil {
			h.log.Warn("poll: skipping malformed event", applog.FieldClientID, clientID, applog.FieldError, err)
			continue
		}
		payloads = append(payloads, p)
	}

	return ctx.JSON(http.StatusOK, map[string]any{
		"events": payloads,
		"count":  len(payloads),
	})
}
