// Package metrics provides a dependency-injectable Prometheus metrics struct for Scorpion.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all registered Prometheus metrics for Scorpion.
type Metrics struct {
	ActiveConnections         prometheus.Gauge
	ConnectionsTotal          *prometheus.CounterVec
	EventsDeliveredTotal      *prometheus.CounterVec
	EventsDrainedPerTick      prometheus.Histogram
	EventDrainErrorsTotal     prometheus.Counter
	TicketsIssuedTotal        prometheus.Counter
	TicketsRejectedTotal      *prometheus.CounterVec
	AuthIPMismatchTotal       prometheus.Counter
	DuplicateConnectionsTotal prometheus.Counter
	HeartbeatsSentTotal       prometheus.Counter
	RedisLatencySeconds       *prometheus.HistogramVec
	RateLimitExceededTotal    prometheus.Counter
}

// NewMetrics registers all Scorpion Prometheus metrics and returns a populated Metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		ActiveConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "scorpion_active_connections",
			Help: "Currently active SSE streams.",
		}),
		ConnectionsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "scorpion_connections_total",
			Help: "Total SSE connection attempts.",
		}, []string{"status"}),
		EventsDeliveredTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "scorpion_events_delivered_total",
			Help: "Total events sent to clients.",
		}, []string{"event_type"}),
		EventsDrainedPerTick: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "scorpion_events_drained_per_tick",
			Help:    "Batch size distribution per poll tick.",
			Buckets: prometheus.LinearBuckets(0, 10, 11),
		}),
		EventDrainErrorsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_event_drain_errors_total",
			Help: "Failed Redis drain operations.",
		}),
		TicketsIssuedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_tickets_issued_total",
			Help: "Total tickets issued.",
		}),
		TicketsRejectedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "scorpion_tickets_rejected_total",
			Help: "Rejected ticket requests.",
		}, []string{"reason"}),
		AuthIPMismatchTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_auth_ip_mismatch_total",
			Help: "SSE connections rejected due to IP mismatch.",
		}),
		DuplicateConnectionsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_duplicate_connections_total",
			Help: "Duplicate stream attempts blocked.",
		}),
		HeartbeatsSentTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_heartbeats_sent_total",
			Help: "Total heartbeat comments sent.",
		}),
		RedisLatencySeconds: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "scorpion_redis_latency_seconds",
			Help:    "Redis operation latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
		RateLimitExceededTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_ratelimit_exceeded_total",
			Help: "Rate limit hits on ticket endpoint.",
		}),
	}
}
