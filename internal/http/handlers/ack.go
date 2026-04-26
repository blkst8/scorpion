package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/blkst8/scorpion/internal/domain"
	"github.com/blkst8/scorpion/internal/http/middleware"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// V1AckEvent handles POST /v1/ack — receives an ACK for a delivered SSE event,
// validates it, marks it in the in-flight registry, and publishes to NATS.
func (h *HTTPHandlers) V1AckEvent(ctx echo.Context) error {
	start := time.Now()

	// ── 1. NATS readiness guard ──────────────────────────────────────────────
	if !h.AckPublisher.Connected() {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]string{
			"error":   "service_unavailable",
			"message": "ACK handler is not connected to NATS.",
		})
	}

	// ── 2. Authenticated client_id from middleware ───────────────────────────
	authedClientID, _ := ctx.Get(middleware.ClientID).(string)

	// ── 3. Parse request body ─────────────────────────────────────────────────
	var req domain.AckRequest
	if err := sonic.ConfigDefault.NewDecoder(ctx.Request().Body).Decode(&req); err != nil {
		h.Metrics.AckValidationErrorsTotal.WithLabelValues("malformed_json").Inc()
		return ctx.JSON(http.StatusBadRequest, map[string]string{
			"error":   "bad_request",
			"message": "Malformed or non-JSON request body.",
		})
	}

	// ── 4. Field validation ───────────────────────────────────────────────────
	if err := validateAckRequest(&req); err != nil {
		h.Metrics.AckValidationErrorsTotal.WithLabelValues("missing_fields").Inc()
		return ctx.JSON(http.StatusBadRequest, map[string]string{
			"error":   "bad_request",
			"message": err.Error(),
		})
	}

	// ── 5. Authorization — client_id must match JWT subject ──────────────────
	if req.ClientID != authedClientID {
		h.Metrics.AckValidationErrorsTotal.WithLabelValues("client_id_mismatch").Inc()
		h.Log.Warn("ACK client_id mismatch",
			applog.FieldClientID, req.ClientID,
			"jwt_subject", authedClientID,
		)
		return ctx.JSON(http.StatusForbidden, map[string]string{
			"error":   "forbidden",
			"message": "client_id does not match authorization token.",
		})
	}

	// ── 6. In-flight validation ───────────────────────────────────────────────
	found, alreadyAcked, err := h.InFlight.Validate(ctx.Request().Context(), req.ClientID, req.EventID)
	if err != nil {
		h.Log.Error("inflight validate error",
			applog.FieldClientID, req.ClientID,
			applog.FieldError, err,
		)
		return ctx.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "internal_error",
			"message": "Failed to validate event.",
		})
	}

	if alreadyAcked {
		h.Metrics.AckDuplicateTotal.WithLabelValues(req.StreamID).Inc()
		h.Metrics.AckRequestsTotal.WithLabelValues(strconv.Itoa(http.StatusConflict), req.StreamID).Inc()
		return ctx.JSON(http.StatusConflict, map[string]string{
			"error":   "conflict",
			"message": "This event has already been acknowledged.",
		})
	}

	if !found {
		h.Metrics.AckUnknownEventTotal.WithLabelValues(req.StreamID).Inc()
		h.Metrics.AckRequestsTotal.WithLabelValues(strconv.Itoa(http.StatusNotFound), req.StreamID).Inc()
		return ctx.JSON(http.StatusNotFound, map[string]string{
			"error":   "not_found",
			"message": "event_id not found in in-flight registry.",
		})
	}

	// ── 7. Build ACK record ───────────────────────────────────────────────────
	serverNow := time.Now().UTC()
	ackID := uuid.New().String()
	record := domain.AckRecord{
		AckID:            ackID,
		EventID:          req.EventID,
		ClientID:         req.ClientID,
		StreamID:         req.StreamID,
		AckedAt:          req.AckedAt,
		Status:           req.Status,
		Metadata:         req.Metadata,
		ServerReceivedAt: serverNow,
	}

	// ── 8. Publish to NATS ────────────────────────────────────────────────────
	publishStart := time.Now()
	result, err := h.AckPublisher.Publish(ctx.Request().Context(), record)
	publishDuration := time.Since(publishStart).Seconds()

	subject := fmt.Sprintf("scorpion.sse.acks.%s", req.ClientID)
	if err != nil {
		h.Metrics.NATSPublishTotal.WithLabelValues(subject, "failure").Inc()
		h.Metrics.AckRequestsTotal.WithLabelValues(strconv.Itoa(http.StatusBadGateway), req.StreamID).Inc()
		h.Log.Error("NATS publish failed",
			applog.FieldClientID, req.ClientID,
			applog.FieldError, err,
		)
		return ctx.JSON(http.StatusBadGateway, map[string]string{
			"error":   "bad_gateway",
			"message": "Failed to publish ACK to NATS.",
		})
	}
	h.Metrics.NATSPublishTotal.WithLabelValues(subject, "success").Inc()
	h.Metrics.NATSPublishDuration.WithLabelValues(subject).Observe(publishDuration)

	// ── 9. Mark as acked in Redis ─────────────────────────────────────────────
	if _, err := h.InFlight.MarkAcked(ctx.Request().Context(), req.ClientID, req.EventID); err != nil {
		// Non-fatal: the ACK was published to NATS; log and continue.
		h.Log.Warn("mark acked failed (non-fatal)",
			applog.FieldClientID, req.ClientID,
			applog.FieldError, err,
		)
	}

	// ── 10. Metrics & response ────────────────────────────────────────────────
	h.Metrics.AckProcessingDuration.WithLabelValues(req.StreamID).Observe(time.Since(start).Seconds())
	h.Metrics.AckRequestsTotal.WithLabelValues(strconv.Itoa(http.StatusOK), req.StreamID).Inc()
	h.Metrics.UnackedEventsTotal.WithLabelValues(req.StreamID, req.ClientID).Dec()

	return ctx.JSON(http.StatusOK, domain.AckResponse{
		AckID:     ackID,
		EventID:   req.EventID,
		Published: true,
		NATSSeq:   result.Seq,
		Timestamp: serverNow,
	})
}

// validateAckRequest checks required fields and status enum.
func validateAckRequest(r *domain.AckRequest) error {
	switch {
	case r.EventID == "":
		return fmt.Errorf("missing required field: event_id")
	case r.ClientID == "":
		return fmt.Errorf("missing required field: client_id")
	case r.StreamID == "":
		return fmt.Errorf("missing required field: stream_id")
	case r.AckedAt.IsZero():
		return fmt.Errorf("missing required field: acked_at")
	case r.Status != domain.AckStatusReceived && r.Status != domain.AckStatusProcessed && r.Status != domain.AckStatusFailed:
		return fmt.Errorf("invalid status: must be one of received, processed, failed")
	}
	return nil
}
