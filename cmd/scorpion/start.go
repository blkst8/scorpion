package scorpion

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	_ "go.uber.org/automaxprocs"
	"golang.org/x/net/http2"

	"github.com/blkst8/scorpion/internal/auth"
	"github.com/blkst8/scorpion/internal/config"
	appmiddleware "github.com/blkst8/scorpion/internal/middleware"
	"github.com/blkst8/scorpion/internal/observability"
	"github.com/blkst8/scorpion/internal/ratelimit"
	redisstore "github.com/blkst8/scorpion/internal/redis"
	"github.com/blkst8/scorpion/internal/stream"
)

var configPath string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Scorpion SSE server",
	RunE:  runStart,
}

func init() {
	startCmd.Flags().StringVar(&configPath, "config", "config/config.yaml", "Path to config file")
	rootCmd.AddCommand(startCmd)
}

func runStart(_ *cobra.Command, _ []string) error {
	// Load configuration
	if err := config.Load(configPath); err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	cfg := config.C

	// Initialize logger
	observability.InitLogger(cfg.Observability)
	log := observability.Logger

	log.Info("scorpion starting",
		"version", "0.3.0",
		"port", cfg.Server.Port,
	)

	// Build IP strategy
	ipStrategy, err := appmiddleware.NewIPStrategy(cfg.IP)
	if err != nil {
		return fmt.Errorf("failed to initialize IP strategy: %w", err)
	}

	// Connect to Redis
	rdb, err := redisstore.NewClient(cfg.Redis)
	if err != nil {
		return fmt.Errorf("failed to connect to redis: %w", err)
	}
	defer rdb.Close()

	// Build stores
	instanceID := uuid.NewString()
	ticketStore := redisstore.NewTicketStore(rdb)
	connStore := redisstore.NewConnectionStore(rdb, instanceID)
	eventStore := redisstore.NewEventStore(rdb)
	limiter := ratelimit.NewLimiter(rdb, cfg.RateLimit)

	// Build handlers
	ticketHandler := auth.NewTicketHandler(*cfg, ticketStore, limiter, ipStrategy)
	sseHandler := stream.NewSSEHandler(*cfg, ticketStore, connStore, eventStore, ipStrategy)

	// Set up Echo
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.Use(echomiddleware.Recover())
	e.Use(echomiddleware.RequestID())

	// Routes
	e.GET("/healthz", observability.HealthHandler(rdb, func() float64 {
		// Read active connections gauge value - use a simple approximation
		return 0 // Prometheus gauge is the source of truth
	}))

	v1 := e.Group("/v1")
	v1.POST("/auth/ticket", ticketHandler.Handle)
	v1.GET("/stream/events", sseHandler.Handle)

	// TLS config for HTTP/2
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	mainSrv := &http.Server{
		Addr:      fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:   e,
		TLSConfig: tlsCfg,
	}

	// Enable HTTP/2
	if err := http2.ConfigureServer(mainSrv, &http2.Server{}); err != nil {
		return fmt.Errorf("failed to configure http2: %w", err)
	}

	// Metrics server on separate port
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Observability.MetricsPort),
		Handler: metricsMux,
	}

	go func() {
		log.Info("metrics server starting", "port", cfg.Observability.MetricsPort)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server error", "error", err)
		}
	}()

	// Start main server
	go func() {
		log.Info("scorpion listening", "port", cfg.Server.Port, "tls", true)
		if err := mainSrv.ListenAndServeTLS(cfg.Server.TLSCert, cfg.Server.TLSKey); err != nil &&
			err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop

	log.Info("shutdown signal received", "signal", sig.String())
	shutdownStart := time.Now()

	// Graceful shutdown — cancels all active request contexts
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := mainSrv.Shutdown(ctx); err != nil {
		log.Error("forced shutdown", "error", err)
	}

	// Bulk cleanup of this instance's connection keys
	connStore.CleanupInstance(context.Background())

	// Shutdown metrics server
	metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer metricsCancel()
	metricsSrv.Shutdown(metricsCtx)

	log.Info("scorpion stopped", "duration", time.Since(shutdownStart).String())
	return nil
}
