// Package domain holds shared domain types for Scorpion.
package domain

import "encoding/json"

// EventPayload is the canonical SSE event structure stored in Redis and
// delivered to clients. Data is kept as raw JSON bytes to avoid double
// encoding.
type EventPayload struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// EventRequest is the inbound payload accepted by the event-push endpoint.
// The ID field is intentionally absent — it is always generated server-side.
type EventRequest struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}
