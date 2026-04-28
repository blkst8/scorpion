// Package log provides structured logger construction for Scorpion.
package log

import (
	"log/slog"
	"os"

	"github.com/blkst8/scorpion/internal/config"
)

// NewLogger builds a *slog.Logger from configuration and sets it as the default.
func NewLogger(cfg config.Observability) *slog.Logger {
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

	opts := &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	l := slog.New(handler)
	slog.SetDefault(l)
	return l
}
