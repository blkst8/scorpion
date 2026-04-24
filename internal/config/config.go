// Package config provides configuration loading and access for Scorpion.
package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// Config is the top-level configuration structure.
type Config struct {
	Server        Server        `yaml:"server"`
	SSE           SSE           `yaml:"sse"`
	Auth          Auth          `yaml:"auth"`
	IP            IP            `yaml:"ip"`
	RateLimit     RateLimit     `yaml:"ratelimit"`
	Redis         Redis         `yaml:"repository"`
	Observability Observability `yaml:"observability"`
}

// Server contains HTTP server settings.
type Server struct {
	Port            int           `yaml:"port"`
	TLSCert         string        `yaml:"tls_cert"`
	TLSKey          string        `yaml:"tls_key"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
}

// SSE contains Server-Sent Events configuration.
type SSE struct {
	PollInterval      time.Duration `yaml:"poll_interval"`
	BatchSize         int           `yaml:"batch_size"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	ConnTTL           time.Duration `yaml:"conn_ttl"`
	MaxQueueDepth     int64         `yaml:"max_queue_depth"`
	MaxEventBytes     int           `yaml:"max_event_bytes"`
}

// Auth contains authentication configuration.
type Auth struct {
	TokenSecret  string        `yaml:"token_secret"`
	TicketSecret string        `yaml:"ticket_secret"`
	TicketTTL    time.Duration `yaml:"ticket_ttl"`
}

// IP contains IP extraction configuration.
type IP struct {
	Strategy      string   `yaml:"strategy"`
	Header        string   `yaml:"header"`
	TrustedCount  int      `yaml:"trusted_count"`
	TrustedRanges []string `yaml:"trusted_ranges"`
}

// RateLimit contains rate limiting configuration.
type RateLimit struct {
	TicketRPM   int `yaml:"ticket_rpm"`
	TicketBurst int `yaml:"ticket_burst"`
}

// Redis contains Redis connection configuration.
type Redis struct {
	Address      string `yaml:"address"`
	Password     string `yaml:"password"`
	DB           int    `yaml:"db"`
	MaxRetries   int    `yaml:"max_retries"`
	PoolSize     int    `yaml:"pool_size"`      // 0 = go-app default (10 × GOMAXPROCS)
	MinIdleConns int    `yaml:"min_idle_conns"` // 0 = go-app default
}

// Observability contains metrics, logging, and tracing configuration.
type Observability struct {
	MetricsPort    int    `yaml:"metrics_port"`
	LogLevel       string `yaml:"log_level"`
	LogFormat      string `yaml:"log_format"`
	TracingEnabled bool   `yaml:"tracing_enabled"`
	OTLPEndpoint   string `yaml:"otlp_endpoint"`
}

// Load reads configuration from the given file path and returns a populated Config.
func Load(configPath string) (*Config, error) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	// Defaults
	viper.SetDefault("server.port", 8443)
	viper.SetDefault("server.shutdown_timeout", "30s")
	viper.SetDefault("server.read_timeout", "10s")
	viper.SetDefault("server.idle_timeout", "60s")
	viper.SetDefault("sse.poll_interval", "1s")
	viper.SetDefault("sse.batch_size", 100)
	viper.SetDefault("sse.heartbeat_interval", "15s")
	viper.SetDefault("sse.conn_ttl", "60s")
	viper.SetDefault("sse.max_queue_depth", 10000)
	viper.SetDefault("sse.max_event_bytes", 65536) // 64 KiB
	viper.SetDefault("auth.ticket_ttl", "5m")
	viper.SetDefault("ip.strategy", "remote_addr")
	viper.SetDefault("ip.header", "X-Forwarded-For")
	viper.SetDefault("ratelimit.ticket_rpm", 10)
	viper.SetDefault("ratelimit.ticket_burst", 3)
	viper.SetDefault("repository.address", "localhost:6379")
	viper.SetDefault("repository.db", 0)
	viper.SetDefault("repository.max_retries", 3)
	viper.SetDefault("observability.metrics_port", 9090)
	viper.SetDefault("observability.log_level", "info")
	viper.SetDefault("observability.log_format", "json")
	viper.SetDefault("observability.tracing_enabled", false)

	// Environment variable overrides for secrets and critical settings.
	_ = viper.BindEnv("auth.token_secret", "SCORPION_AUTH_TOKEN_SECRET")
	_ = viper.BindEnv("auth.ticket_secret", "SCORPION_AUTH_TICKET_SECRET")
	_ = viper.BindEnv("repository.password", "SCORPION_REDIS_PASSWORD")
	_ = viper.BindEnv("observability.otlp_endpoint", "SCORPION_OTLP_ENDPOINT")

	// Environment variable overrides
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	C := &Config{}
	if err := viper.Unmarshal(C); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return C, nil
}
