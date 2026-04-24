package handlers

import (
	"log/slog"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/ratelimit"
	"github.com/blkst8/scorpion/internal/repository"
	"github.com/realclientip/realclientip-go"
	"github.com/redis/go-redis/v9"
)

type HTTPHandlers struct {
	RDB        *redis.Client
	Events     repository.EventStore
	Tickets    repository.TicketStore
	Conns      repository.ConnectionStore
	Limiter    ratelimit.Limiter
	IPStrategy realclientip.Strategy
	Cfg        *config.Config
	Log        *slog.Logger
	Metrics    *metrics.Metrics
}
