package stream

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/blkst8/scorpion/internal/observability"
)

// sendHeartbeat writes an SSE comment heartbeat to keep the connection alive.
func sendHeartbeat(w io.Writer, flusher http.Flusher, clientID, clientIP string) {
	fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix())
	flusher.Flush()

	observability.HeartbeatsSentTotal.Inc()
	observability.Logger.Debug("heartbeat sent", "client_id", clientID, "ip", clientIP)
}
