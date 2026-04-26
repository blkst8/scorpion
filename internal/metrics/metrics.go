// Package metrics provides a dependency-injectable Prometheus metrics struct for Scorpion.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all registered Prometheus metrics for Scorpion.
type Metrics struct {
	// SSE Publisher metrics.
	ActiveConnections         prometheus.Gauge
	ConnectionsTotal          *prometheus.CounterVec
	EventsDeliveredTotal      *prometheus.CounterVec
	EventsDrainedPerTick      prometheus.Histogram
	EventDrainErrorsTotal     prometheus.Counter
	QueueDepth                prometheus.Gauge
	TicketsIssuedTotal        prometheus.Counter
	TicketsRejectedTotal      *prometheus.CounterVec
	AuthIPMismatchTotal       prometheus.Counter
	DuplicateConnectionsTotal prometheus.Counter
	HeartbeatsSentTotal       prometheus.Counter
	RedisLatencySeconds       *prometheus.HistogramVec
	RateLimitExceededTotal    prometheus.Counter
	UnackedEventsTotal        *prometheus.GaugeVec

	// ACK Handler metrics.
	AckRequestsTotal         *prometheus.CounterVec
	AckProcessingDuration    *prometheus.HistogramVec
	AckValidationErrorsTotal *prometheus.CounterVec
	AckDuplicateTotal        *prometheus.CounterVec
	AckToEventLatency        *prometheus.HistogramVec
	AckUnknownEventTotal     *prometheus.CounterVec

	// NATS publisher metrics.
	NATSPublishTotal        *prometheus.CounterVec
	NATSPublishDuration     *prometheus.HistogramVec
	NATSPublishRetriesTotal *prometheus.CounterVec
	NATSConnectionStatus    *prometheus.GaugeVec
}

// NewMetrics registers all Scorpion Prometheus metrics against reg.
// Pass prometheus.DefaultRegisterer for production, prometheus.NewRegistry()
// in tests to avoid duplicate-registration panics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		ActiveConnections: f.NewGauge(prometheus.GaugeOpts{
			Name: "scorpion_active_connections",
			Help: "Currently active SSE streams.",
		}),
		ConnectionsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "scorpion_connections_total",
			Help: "Total SSE connection attempts.",
		}, []string{"status"}),
		EventsDeliveredTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "scorpion_events_delivered_total",
			Help: "Total events sent to clients.",
		}, []string{"event_type"}),
		EventsDrainedPerTick: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "scorpion_events_drained_per_tick",
			Help:    "Batch size distribution per poll tick.",
			Buckets: prometheus.LinearBuckets(0, 10, 11),
		}),
		EventDrainErrorsTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_event_drain_errors_total",
			Help: "Failed Redis drain operations.",
		}),
		QueueDepth: f.NewGauge(prometheus.GaugeOpts{
			Name: "scorpion_queue_depth",
			Help: "Approximate number of pending events in the Redis queue after the last drain.",
		}),
		TicketsIssuedTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_tickets_issued_total",
			Help: "Total tickets issued.",
		}),
		TicketsRejectedTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "scorpion_tickets_rejected_total",
			Help: "Rejected ticket requests.",
		}, []string{"reason"}),
		AuthIPMismatchTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_auth_ip_mismatch_total",
			Help: "SSE connections rejected due to IP mismatch.",
		}),
		DuplicateConnectionsTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_duplicate_connections_total",
			Help: "Duplicate stream attempts blocked.",
		}),
		HeartbeatsSentTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_heartbeats_sent_total",
			Help: "Total heartbeat comments sent.",
		}),
		RedisLatencySeconds: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "scorpion_redis_latency_seconds",
			Help:    "Redis operation latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
		RateLimitExceededTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "scorpion_ratelimit_exceeded_total",
			Help: "Rate limit hits on ticket endpoint.",
		}),
		UnackedEventsTotal: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sse_unacked_events_total",
			Help: "Count of events delivered but not yet acknowledged.",
		}, []string{"stream_id", "client_id"}),

		// ACK Handler metrics.
		AckRequestsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ack_requests_total",
			Help: "Total ACK HTTP requests received.",
		}, []string{"http_status", "stream_id"}),
		AckProcessingDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ack_processing_duration_seconds",
			Help:    "End-to-end ACK processing latency from receipt to NATS publish.",
			Buckets: prometheus.DefBuckets,
		}, []string{"stream_id"}),
		AckValidationErrorsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ack_validation_errors_total",
			Help: "Count of ACK payload validation failures.",
		}, []string{"error_type"}),
		AckDuplicateTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ack_duplicate_total",
			Help: "Count of duplicate ACK attempts rejected.",
		}, []string{"stream_id"}),
		AckToEventLatency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ack_to_event_latency_seconds",
			Help:    "Time between event server-push and ACK receipt.",
			Buckets: prometheus.DefBuckets,
		}, []string{"stream_id"}),
		AckUnknownEventTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ack_unknown_event_total",
			Help: "ACKs rejected because event_id was not found.",
		}, []string{"stream_id"}),

		// NATS publisher metrics.
		NATSPublishTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nats_publish_total",
			Help: "Total NATS publish attempts.",
		}, []string{"subject", "status"}),
		NATSPublishDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nats_publish_duration_seconds",
			Help:    "Round-trip latency of a NATS JetStream publish call.",
			Buckets: prometheus.DefBuckets,
		}, []string{"subject"}),
		NATSPublishRetriesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nats_publish_retries_total",
			Help: "Total NATS publish retry attempts.",
		}, []string{"subject"}),
		NATSConnectionStatus: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nats_connection_status",
			Help: "NATS connection health: 1=connected, 0=disconnected.",
		}, []string{"server"}),
	}
}
