package scorpion

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	_ "go.uber.org/automaxprocs"

	"github.com/blkst8/scorpion/internal/appmiddleware"
	"github.com/blkst8/scorpion/internal/config"
	httpserver "github.com/blkst8/scorpion/internal/http"
	"github.com/blkst8/scorpion/internal/http/handlers"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/ratelimit"
	redisstore "github.com/blkst8/scorpion/internal/repository"
	"github.com/blkst8/scorpion/internal/telemetry"
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
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	log := applog.NewLogger(cfg.Observability)
	m := metrics.NewMetrics(prometheus.DefaultRegisterer)

	log.Info("scorpion starting",
		applog.FieldVersion, "0.4.0",
		applog.FieldPort, cfg.Server.Port,
	)

	// Tracing
	shutdownTracing := telemetry.InitTracer(cfg.Observability, log)
	defer shutdownTracing()

	ipStrategy, err := appmiddleware.NewIPStrategy(cfg.IP)
	if err != nil {
		return fmt.Errorf("failed to initialize IP strategy: %w", err)
	}

	rdb, err := redisstore.NewClient(cfg.Redis, m)
	if err != nil {
		return fmt.Errorf("failed to connect to repository: %w", err)
	}
	defer func() { _ = rdb.Close() }()

	instanceID := uuid.NewString()
	ticketStore := redisstore.NewTicketStore(rdb)
	connStore := redisstore.NewConnectionStore(rdb, instanceID, log)
	eventStore := redisstore.NewEventStore(rdb, cfg.SSE.MaxQueueDepth)
	limiter := ratelimit.NewLimiter(rdb, cfg.RateLimit)

	ticketHandler := handlers.NewTicketHandler(*cfg, ticketStore, limiter, ipStrategy, log, m)
	sseHandler := handlers.NewSSEHandler(*cfg, ticketStore, connStore, eventStore, ipStrategy, log, m)
	eventHandler := handlers.NewEventHandler(eventStore, cfg.SSE, log)
	pollHandler := handlers.NewPollHandler(eventStore, cfg.SSE, log)

	srv := httpserver.NewServer(*cfg, log, rdb, ipStrategy, ticketHandler, sseHandler, eventHandler, pollHandler)
	srv.Serve()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop

	log.Info("shutdown signal received", applog.FieldSignal, sig.String())
	shutdownStart := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("forced shutdown", applog.FieldError, err)
	}

	connStore.CleanupInstance(context.Background())

	log.Info("scorpion stopped", applog.FieldDuration, time.Since(shutdownStart).String())
	return nil
}
