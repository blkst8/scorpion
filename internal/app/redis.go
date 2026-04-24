package app

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/metrics"
)

// latencyHook instruments every Redis command with the RedisLatencySeconds histogram.
type latencyHook struct {
	hist *prometheus.HistogramVec
}

func (h latencyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h latencyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		h.hist.WithLabelValues(cmd.Name()).Observe(time.Since(start).Seconds())
		return err
	}
}

func (h latencyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		h.hist.WithLabelValues("pipeline").Observe(time.Since(start).Seconds())
		return err
	}
}

// NewClient creates and validates a new Redis client from configuration.
// If m is non-nil, a latency hook is attached to instrument all commands.
func NewClient(cfg config.Redis, m *metrics.Metrics) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:            cfg.Address,
		Password:        cfg.Password,
		DB:              cfg.DB,
		MaxRetries:      cfg.MaxRetries,
		MinRetryBackoff: 8 * time.Millisecond,
		MaxRetryBackoff: 512 * time.Millisecond,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
	})

	if m != nil {
		rdb.AddHook(latencyHook{hist: m.RedisLatencySeconds})
	}

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("failed to connect to repository: %w", err)
	}

	return rdb, nil
}
