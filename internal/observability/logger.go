// Package observability provides structured logging, metrics, and health check for Scorpion.
package observability

import (
	"log/slog"
	"os"

	"github.com/blkst8/scorpion/internal/config"
)

// Logger is the application-wide structured logger.
var Logger *slog.Logger

// InitLogger initializes the global structured logger based on config.
func InitLogger(cfg config.Observability) {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	Logger = slog.New(handler)
	slog.SetDefault(Logger)
}
