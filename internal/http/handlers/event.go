package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	redisstore "github.com/blkst8/scorpion/internal/repository"
)

type eventPayload struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type eventRequest struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// EventHandler handles POST /v1/events/:client_id.
type EventHandler struct {
	events *redisstore.EventStore
	log    *slog.Logger
}

// NewEventHandler creates a new EventHandler with all dependencies injected.
func NewEventHandler(events *redisstore.EventStore, log *slog.Logger) *EventHandler {
	return &EventHandler{
		events: events,
		log:    log,
	}
}

// Handle processes POST /v1/events/:client_id.
func (h *EventHandler) Handle(ctx echo.Context) error {
	clientID := ctx.Param("client_id")
	if clientID == "" {
		return ctx.JSON(http.StatusBadRequest, errorResponse{
			Error:   "bad_request",
			Message: "Missing client_id.",
		})
	}

	var req eventRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(http.StatusBadRequest, errorResponse{
			Error:   "bad_request",
			Message: "Invalid request body.",
		})
	}

	if req.Type == "" {
		return ctx.JSON(http.StatusBadRequest, errorResponse{
			Error:   "bad_request",
			Message: "Field 'type' is required.",
		})
	}

	payload := eventPayload{
		ID:   uuid.NewString(),
		Type: req.Type,
		Data: req.Data,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		h.log.Error("failed to marshal event payload", "error", err)
		return ctx.JSON(http.StatusInternalServerError, errorResponse{
			Error:   "internal_error",
			Message: "Failed to encode event.",
		})
	}

	if err := h.events.Push(ctx.Request().Context(), clientID, string(raw)); err != nil {
		h.log.Error("failed to push event", "client_id", clientID, "error", err)
		return ctx.JSON(http.StatusInternalServerError, errorResponse{
			Error:   "internal_error",
			Message: "Failed to push event.",
		})
	}

	h.log.Info("event pushed", "client_id", clientID, "event_id", payload.ID, "type", payload.Type)

	return ctx.JSON(http.StatusCreated, payload)
}
