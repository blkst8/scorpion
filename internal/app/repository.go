package app

import (
	"log/slog"

	"github.com/blkst8/scorpion/internal/repository"
	"github.com/redis/go-redis/v9"
)

type Repository struct {
	ConnectionStore repository.ConnectionStore
	EventStore      repository.EventStore
	TicketStore     repository.TicketStore
	InFlightStore   repository.InFlightStore
}

func WithRepository(
	rdb *redis.Client,
	log *slog.Logger,
	instanceID string,
	maxQueueDepth int64,
) *Repository {
	r := new(Repository)
	r.ConnectionStore = repository.NewConnectionStore(rdb, instanceID, log)
	r.EventStore = repository.NewEventStore(rdb, maxQueueDepth)
	r.TicketStore = repository.NewTicketStore(rdb)
	r.InFlightStore = repository.NewInFlightStore(rdb)

	return r
}
