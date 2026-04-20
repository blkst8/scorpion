package stream

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/blkst8/scorpion/internal/metrics"
)

// sendHeartbeat writes an SSE comment heartbeat to keep the connection alive.
func sendHeartbeat(w io.Writer, flusher http.Flusher, clientID, clientIP string, log *slog.Logger, m *metrics.Metrics) {
	fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix())
	flusher.Flush()

	m.HeartbeatsSentTotal.Inc()

	log.Debug(
		"heartbeat sent",
		"client_id", clientID,
		"ip", clientIP,
	)
}
