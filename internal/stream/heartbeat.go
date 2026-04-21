package stream

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
)

// sendHeartbeat writes an SSE comment heartbeat to keep the connection alive.
// Returns an error if the write fails, indicating the client has gone away.
func sendHeartbeat(w io.Writer, rc *http.ResponseController, flusher http.Flusher, clientID, clientIP string, log *slog.Logger, m *metrics.Metrics) error {
	_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	if _, err := fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix()); err != nil {
		return err
	}
	flusher.Flush()

	m.HeartbeatsSentTotal.Inc()

	log.Debug("heartbeat sent",
		applog.FieldClientID, clientID,
		applog.FieldIP, clientIP,
	)

	return nil
}
