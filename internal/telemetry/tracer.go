// Package telemetry provides a minimal tracing abstraction for Scorpion.
// When tracing is disabled (default) everything is a no-op with zero overhead.
// When enabled, spans are emitted as structured log lines so the feature works
// without any external dependency; swap the backend for a real OTLP exporter
// once the otel packages are available in vendor.
package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/blkst8/scorpion/internal/config"
)

// Span is a minimal tracing span.
type Span interface {
	End()
	SetError(err error)
}

// Tracer creates spans.
type Tracer interface {
	Start(ctx context.Context, name string, attrs ...Attr) (context.Context, Span)
}

// Attr is a key-value pair attached to a span.
type Attr struct {
	Key   string
	Value string
}

// StringAttr constructs a string Attr.
func StringAttr(key, value string) Attr { return Attr{Key: key, Value: value} }

// ── no-op implementation ──────────────────────────────────────────────────────

type noopSpan struct{}

func (noopSpan) End()             {}
func (noopSpan) SetError(_ error) {}

type noopTracer struct{}

func (noopTracer) Start(ctx context.Context, _ string, _ ...Attr) (context.Context, Span) {
	return ctx, noopSpan{}
}

// ── log-based implementation ──────────────────────────────────────────────────

type logSpan struct {
	name  string
	start time.Time
	attrs []Attr
	log   *slog.Logger
}

func (s *logSpan) End() {
	args := []any{"span", s.name, "duration_ms", time.Since(s.start).Milliseconds()}
	for _, a := range s.attrs {
		args = append(args, a.Key, a.Value)
	}
	s.log.Debug("span", args...)
}

func (s *logSpan) SetError(err error) {
	if err != nil {
		s.log.Debug("span error", "span", s.name, "error", err)
	}
}

type logTracer struct{ log *slog.Logger }

func (t logTracer) Start(ctx context.Context, name string, attrs ...Attr) (context.Context, Span) {
	return ctx, &logSpan{name: name, start: time.Now(), attrs: attrs, log: t.log}
}

// ── global tracer ─────────────────────────────────────────────────────────────

// Global is the application-level tracer; replaced by InitTracer at startup.
var Global Tracer = noopTracer{}

// InitTracer configures the global tracer from config and returns a no-op
// shutdown function. Tracing is emitted as debug log lines; replace this with
// a real OTLP exporter when the packages are available.
func InitTracer(cfg config.Observability, log *slog.Logger) func() {
	if !cfg.TracingEnabled {
		Global = noopTracer{}
		return func() {}
	}
	Global = logTracer{log: log}
	return func() {}
}
