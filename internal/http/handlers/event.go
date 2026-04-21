package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/domain"
	"github.com/blkst8/scorpion/internal/http/middleware"
	applog "github.com/blkst8/scorpion/internal/log"
	redisstore "github.com/blkst8/scorpion/internal/repository"
	"github.com/blkst8/scorpion/internal/telemetry"
)

// EventHandler handles POST /v1/events/:client_id.
type EventHandler struct {
	events *redisstore.EventStore
	cfg    config.SSE
	log    *slog.Logger
}

// NewEventHandler creates a new EventHandler with all dependencies injected.
func NewEventHandler(events *redisstore.EventStore, cfg config.SSE, log *slog.Logger) *EventHandler {
	return &EventHandler{events: events, cfg: cfg, log: log}
}

// Handle processes POST /v1/events/:client_id.
// Requires TokenMiddleware; the caller's JWT sub must equal :client_id.
func (h *EventHandler) Handle(ctx echo.Context) error {
	callerID, _ := ctx.Get(middleware.ClientID).(string)
	clientID := ctx.Param("client_id")

	if clientID == "" {
		return ctx.JSON(http.StatusBadRequest, errorResponse{Error: "bad_request", Message: "Missing client_id."})
	}

	if callerID != clientID {
		return ctx.JSON(http.StatusForbidden, errorResponse{Error: "forbidden", Message: "You may only push events to your own queue."})
	}

	if h.cfg.MaxEventBytes > 0 {
		ctx.Request().Body = http.MaxBytesReader(ctx.Response().Writer, ctx.Request().Body, int64(h.cfg.MaxEventBytes))
	}

	var req domain.EventRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, errorResponse{Error: "bad_request", Message: "Invalid request body or payload too large."})
	}

	if req.Type == "" {
		return ctx.JSON(http.StatusBadRequest, errorResponse{Error: "bad_request", Message: "Field 'type' is required."})
	}

	spanCtx, span := telemetry.Global.Start(ctx.Request().Context(), "event.push",
		telemetry.StringAttr(applog.FieldClientID, clientID),
		telemetry.StringAttr(applog.FieldEventType, req.Type),
	)
	defer span.End()

	payload := domain.EventPayload{
		ID:   uuid.NewString(),
		Type: req.Type,
		Data: req.Data,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		h.log.Error("failed to marshal event payload", applog.FieldClientID, clientID, applog.FieldError, err)
		return ctx.JSON(http.StatusInternalServerError, errorResponse{Error: "internal_error", Message: "Failed to encode event."})
	}

	if err := h.events.Push(spanCtx, clientID, string(raw)); err != nil {
		h.log.Error("failed to push event", applog.FieldClientID, clientID, applog.FieldError, err)
		return ctx.JSON(http.StatusInternalServerError, errorResponse{Error: "internal_error", Message: "Failed to push event."})
	}

	h.log.Info("event pushed",
		applog.FieldClientID, clientID,
		applog.FieldEventID, payload.ID,
		applog.FieldEventType, payload.Type,
	)

	return ctx.JSON(http.StatusCreated, payload)
}
