// Package log provides structured logger construction for Scorpion.
package log

// Log field name constants — use these everywhere instead of bare string
// literals to guarantee snake_case consistency across the whole codebase.
const (
	FieldClientID  = "client_id"
	FieldIP        = "ip"
	FieldTicketIP  = "ticket_ip"
	FieldRequestIP = "request_ip"
	FieldJTI       = "jti"
	FieldError     = "error"
	FieldEventID   = "event_id"
	FieldEventType = "event_type"
	FieldRaw       = "raw"
	FieldCount     = "count"
	FieldLimit     = "limit"
	FieldResetAt   = "reset_at"
	FieldDuration  = "duration"
	FieldPort      = "port"
	FieldSignal    = "signal"
	FieldVersion   = "version"
)
