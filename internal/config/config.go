// Package config provides configuration loading and access for Scorpion.
package config

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/spf13/viper"
	"github.com/tidwall/pretty"
)

const envPrefix = "scorpion"

// Config is the top-level configuration structure.
type Config struct {
	Server        Server        `yaml:"server" json:"server"`
	SSE           SSE           `yaml:"sse" json:"sse"`
	Auth          Auth          `yaml:"auth" json:"auth"`
	IP            IP            `yaml:"ip" json:"IP"`
	RateLimit     RateLimit     `yaml:"ratelimit" json:"rateLimit"`
	Redis         Redis         `yaml:"redis" json:"redis"`
	NATS          NATS          `yaml:"nats" json:"NATS"`
	Observability Observability `yaml:"observability" json:"observability"`
}

// Server contains HTTP server settings.
type Server struct {
	Port            int           `mapstructure:"port" yaml:"port" json:"port"`
	TLSCert         string        `mapstructure:"tls_cert" yaml:"tls_cert" json:"tls_cert"`
	TLSKey          string        `mapstructure:"tls_key" yaml:"tls_key" json:"tls_key"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" yaml:"shutdown_timeout" json:"shutdown_timeout"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout" yaml:"read_timeout" json:"read_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout" yaml:"idle_timeout" json:"idle_timeout"`
}

// SSE contains Server-Sent Events configuration.
type SSE struct {
	PollInterval      time.Duration `mapstructure:"poll_interval" yaml:"poll_interval" json:"poll_interval"`
	BatchSize         int           `mapstructure:"batch_size" yaml:"batch_size" json:"batch_size"`
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval" yaml:"heartbeat_interval" json:"heartbeat_interval"`
	ConnTTL           time.Duration `mapstructure:"conn_ttl" yaml:"conn_ttl" json:"conn_ttl"`
	MaxQueueDepth     int64         `mapstructure:"max_queue_depth" yaml:"max_queue_depth" json:"max_queue_depth"`
	MaxEventBytes     int           `mapstructure:"max_event_bytes" yaml:"max_event_bytes" json:"max_event_bytes"`
}

// Auth contains authentication configuration.
type Auth struct {
	TokenSecret  string        `mapstructure:"token_secret" yaml:"token_secret" json:"token_secret"`
	TicketSecret string        `mapstructure:"ticket_secret" yaml:"ticket_secret" json:"ticket_secret"`
	TicketTTL    time.Duration `mapstructure:"ticket_ttl" yaml:"ticket_ttl" json:"ticket_ttl"`
}

// IP contains IP extraction configuration.
type IP struct {
	Strategy      string   `mapstructure:"strategy" yaml:"strategy" json:"strategy"`
	Header        string   `mapstructure:"header" yaml:"header" json:"header"`
	TrustedCount  int      `mapstructure:"trusted_count" yaml:"trusted_count" json:"trusted_count"`
	TrustedRanges []string `mapstructure:"trusted_ranges" yaml:"trusted_ranges" json:"trusted_ranges"`
}

// RateLimit contains rate limiting configuration.
type RateLimit struct {
	TicketRPM   int `mapstructure:"ticket_rpm" yaml:"ticket_rpm" json:"ticket_rpm"`
	TicketBurst int `mapstructure:"ticket_burst" yaml:"ticket_burst" json:"ticket_burst"`
}

// Redis contains Redis connection configuration.
type Redis struct {
	Address      string `mapstructure:"address" yaml:"address" json:"address"`
	Password     string `mapstructure:"password" yaml:"password" json:"password"`
	DB           int    `mapstructure:"db" yaml:"db" json:"db"`
	MaxRetries   int    `mapstructure:"max_retries" yaml:"max_retries" json:"max_retries"`
	PoolSize     int    `mapstructure:"pool_size" yaml:"pool_size" json:"pool_size"`                // 0 = go-app default (10 × GOMAXPROCS)
	MinIdleConns int    `mapstructure:"min_idle_conns" yaml:"min_idle_conns" json:"min_idle_conns"` // 0 = go-app default
}

// Observability contains metrics, logging, and tracing configuration.
type Observability struct {
	MetricsPort    int    `mapstructure:"metrics_port" yaml:"metrics_port" json:"metrics_port"`
	LogLevel       string `mapstructure:"log_level" yaml:"log_level" json:"log_level"`
	LogFormat      string `mapstructure:"log_format" yaml:"log_format" json:"log_format"`
	TracingEnabled bool   `mapstructure:"tracing_enabled" yaml:"tracing_enabled" json:"tracing_enabled"`
	OTLPEndpoint   string `mapstructure:"otlp_endpoint" yaml:"otlp_endpoint" json:"otlp_endpoint"`
}

// NATS contains NATS JetStream configuration for the ACK handler.
type NATS struct {
	URL              string        `mapstructure:"url" yaml:"url" json:"url"`
	StreamName       string        `mapstructure:"stream_name" yaml:"stream_name" json:"stream_name"`
	SubjectPrefix    string        `mapstructure:"subject_prefix" yaml:"subject_prefix" json:"subject_prefix"`
	PublishTimeoutMs int           `mapstructure:"publish_timeout_ms" yaml:"publish_timeout_ms" json:"publish_timeout_ms"`
	MaxRetries       int           `mapstructure:"max_retries" yaml:"max_retries" json:"max_retries"`
	CredentialsFile  string        `mapstructure:"credentials_file" yaml:"credentials_file" json:"credentials_file"`
	DuplicateWindow  time.Duration `mapstructure:"duplicate_window" yaml:"duplicate_window" json:"duplicate_window"`
	Enabled          bool          `mapstructure:"enabled" yaml:"enabled" json:"enabled"`
}

// Load reads configuration from the given file path and returns a populated Config.
func Load(configPath string) (*Config, error) {
	// Set config file path
	v := viper.New()
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.SetConfigFile(configPath)
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	// Environment variable overrides
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	C := new(Config)
	if err := v.Unmarshal(C); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Log config
	indent, err := sonic.MarshalIndent(C, "", "\t")
	if err != nil {
		return C, fmt.Errorf("error marshalling config for display: %w", err)
	}

	indent = pretty.Color(indent, nil)

	log.Println(
		fmt.Sprintf(`
================ Loaded Configuration ================
%s
======================================================
	`,
			string(indent),
		),
	)

	return C, nil
}
