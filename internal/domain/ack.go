// Package domain holds shared domain types for Scorpion.
package domain

import "time"

// AckStatus represents the client's processing status for an acknowledged event.
type AckStatus string

const (
	AckStatusReceived  AckStatus = "received"
	AckStatusProcessed AckStatus = "processed"
	AckStatusFailed    AckStatus = "failed"
)

// AckRequest is the inbound payload for POST /v1/ack.
type AckRequest struct {
	EventID  string            `json:"event_id"`
	ClientID string            `json:"client_id"`
	StreamID string            `json:"stream_id"`
	AckedAt  time.Time         `json:"acked_at"`
	Status   AckStatus         `json:"status"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// AckResponse is the structured response returned from POST /v1/ack.
type AckResponse struct {
	AckID     string    `json:"ack_id"`
	EventID   string    `json:"event_id"`
	Published bool      `json:"published"`
	NATSSeq   uint64    `json:"nats_seq"`
	Timestamp time.Time `json:"timestamp"`
}

// AckRecord is the full record published to NATS JetStream.
type AckRecord struct {
	AckID            string            `json:"ack_id"`
	EventID          string            `json:"event_id"`
	ClientID         string            `json:"client_id"`
	StreamID         string            `json:"stream_id"`
	AckedAt          time.Time         `json:"acked_at"`
	Status           AckStatus         `json:"status"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ServerReceivedAt time.Time         `json:"server_received_at"`
}
