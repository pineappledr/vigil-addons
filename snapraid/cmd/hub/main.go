package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	"github.com/pineappledr/vigil-addons/snapraid/internal/hub"
)

//go:embed manifest.json
var manifestFS embed.FS

func main() {
	configPath := flag.String("config", "config.hub.yaml", "path to hub configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.LoadHubConfig(*configPath)
	if err != nil {
		logger.Error("failed to load hub config", "error", err)
		os.Exit(1)
	}

	if level, err := parseLogLevel(cfg.Logging.Level); err == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		}))
	}

	config.LogHubConfig(logger, cfg)

	registry, err := hub.NewRegistry(cfg.Data.RegistryPath)
	if err != nil {
		logger.Error("failed to initialize agent registry", "error", err)
		os.Exit(1)
	}

	// Read the embedded manifest.
	manifestData, err := manifestFS.ReadFile("manifest.json")
	if err != nil {
		logger.Error("failed to read embedded manifest", "error", err)
		os.Exit(1)
	}

	upstreamCh := make(chan []byte, 256)
	aggregator := hub.NewAggregator(registry, upstreamCh, logger)
	router := hub.NewCommandRouter(registry, logger)

	srv := hub.NewServer(cfg, registry, aggregator, router, logger)

	addr := fmt.Sprintf(":%d", cfg.Listen.Port)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Root context for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Register with the Vigil server in the background, then start telemetry.
	if cfg.Vigil.Token != "" {
		go func() {
			vigilClient, err := hub.NewVigilClient(cfg.Vigil.ServerURL, cfg.Vigil.Token, manifestData, logger)
			if err != nil {
				logger.Error("failed to create vigil client", "error", err)
				return
			}

			resp, err := vigilClient.Register(ctx)
			if err != nil {
				logger.Error("vigil registration abandoned", "error", err)
				return
			}
			logger.Info("vigil registration complete",
				"addon_id", resp.AddonID,
				"session_id", resp.SessionID,
			)

			// Start the persistent upstream telemetry WebSocket.
			telemetry := hub.NewTelemetryClient(
				cfg.Vigil.ServerURL,
				resp.AddonID,
				cfg.Vigil.Token,
				30*time.Second,
				upstreamCh,
				logger,
			)

			go telemetry.Run(ctx)

			select {
			case <-telemetry.Ready():
				logger.Info("upstream telemetry pipeline ready")
			case <-ctx.Done():
				return
			}
		}()
	} else {
		logger.Warn("no vigil token configured, running in standalone mode")
	}

	// Start HTTP server.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("hub listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal or server error.
	select {
	case err := <-errCh:
		logger.Error("server error", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	logger.Info("hub stopped")
}

func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))
	return level, err
}
