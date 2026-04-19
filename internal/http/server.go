// Package httpserver provides the HTTP server setup and lifecycle management for Scorpion.
package httpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"

	"github.com/blkst8/scorpion/internal/config"
	"github.com/blkst8/scorpion/internal/http/handlers"
)

// Server wraps the main TLS Echo server and the separate metrics HTTP server.
type Server struct {
	cfg        config.Config
	log        *slog.Logger
	echo       *echo.Echo
	mainSrv    *http.Server
	metricsSrv *http.Server
}

// NewServer builds and wires an Echo server with all routes registered.
func NewServer(
	cfg config.Config,
	log *slog.Logger,
	rdb *redis.Client,
	ticketHandler *handlers.TicketHandler,
	sseHandler *handlers.SSEHandler,
) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.Use(echomiddleware.Recover())
	e.Use(echomiddleware.RequestID())

	e.GET("/healthz", handlers.HealthHandler(rdb, func() float64 { return 0 }))

	v1 := e.Group("/v1")
	v1.POST("/auth/ticket", ticketHandler.Handle)
	v1.GET("/stream/events", sseHandler.Handle)

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	mainSrv := &http.Server{
		Addr:      fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:   e,
		TLSConfig: tlsCfg,
	}

	if err := http2.ConfigureServer(mainSrv, &http2.Server{}); err != nil {
		log.Error("failed to configure http2", "error", err)
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Observability.MetricsPort),
		Handler: metricsMux,
	}

	return &Server{
		cfg:        cfg,
		log:        log,
		echo:       e,
		mainSrv:    mainSrv,
		metricsSrv: metricsSrv,
	}
}

// Serve starts the metrics server and main TLS server in background goroutines.
func (s *Server) Serve() {
	go func() {
		s.log.Info("metrics server starting", "port", s.cfg.Observability.MetricsPort)
		if err := s.metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("metrics server error", "error", err)
		}
	}()

	go func() {
		s.log.Info("scorpion listening", "port", s.cfg.Server.Port, "tls", true)
		if err := s.mainSrv.ListenAndServeTLS(s.cfg.Server.TLSCert, s.cfg.Server.TLSKey); err != nil &&
			err != http.ErrServerClosed {
			s.log.Error("server error", "error", err)
		}
	}()
}

// Shutdown gracefully stops the main server then the metrics server.
func (s *Server) Shutdown(ctx context.Context) error {
	metricsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.metricsSrv.Shutdown(metricsCtx)
	return s.mainSrv.Shutdown(ctx)
}
