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

	"github.com/pineappledr/vigil-addons/shared/vigilclient"
	"github.com/pineappledr/vigil-addons/zfs-manager/internal/config"
	"github.com/pineappledr/vigil-addons/zfs-manager/internal/manager"
)

//go:embed manifest.json
var manifestFS embed.FS

var version = "dev"

func main() {
	configPath := flag.String("config", "config.manager.yaml", "path to manager config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.LoadManagerConfig(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if level, err := parseLogLevel(cfg.Logging.Level); err == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}

	// Load or generate PSK for agent authentication.
	psk, err := manager.LoadOrGeneratePSK(cfg.Data.RegistryPath)
	if err != nil {
		logger.Error("failed to load or generate PSK", "error", err)
		os.Exit(1)
	}
	logger.Info("PSK ready", "psk_path", manager.PSKPath(cfg.Data.RegistryPath))

	// Load persisted Vigil token if previously rotated.
	if persisted := manager.LoadPersistedToken(cfg.Data.RegistryPath); persisted != "" {
		cfg.Vigil.Token = persisted
	}

	config.LogManagerConfig(logger, cfg)

	registry, err := manager.NewRegistry(cfg.Data.RegistryPath)
	if err != nil {
		logger.Error("failed to initialize registry", "error", err)
		os.Exit(1)
	}

	manifestData, err := manifestFS.ReadFile("manifest.json")
	if err != nil {
		logger.Error("failed to read manifest", "error", err)
		os.Exit(1)
	}

	upstreamCh := make(chan []byte, 256)
	aggregator := manager.NewAggregator(registry, upstreamCh, logger)
	srv := manager.NewServer(cfg, registry, aggregator, psk, logger)

	addr := fmt.Sprintf(":%d", cfg.Listen.Port)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Register with Vigil server and start upstream telemetry.
	if cfg.Vigil.Token != "" {
		go func() {
			vigilClient, err := vigilclient.NewVigilClient(cfg.Vigil.ServerURL, cfg.Vigil.Token, manifestData, logger)
			if err != nil {
				logger.Error("failed to create vigil client", "error", err)
				return
			}

			resp, err := vigilClient.Register(ctx)
			if err != nil {
				logger.Error("vigil registration failed", "error", err)
				return
			}
			logger.Info("vigil registration complete", "addon_id", resp.AddonID)

			telemetry := vigilclient.NewTelemetryClient(
				cfg.Vigil.ServerURL,
				resp.AddonID,
				cfg.Vigil.Token,
				30*time.Second,
				upstreamCh,
				logger,
			)
			aggregator.SetTelemetryClient(telemetry)
			go telemetry.Run(ctx)

			select {
			case <-telemetry.Ready():
				logger.Info("upstream telemetry pipeline ready")
			case <-ctx.Done():
			}
		}()
	} else {
		logger.Warn("no vigil token configured, running in standalone mode")
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("manager listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		logger.Error("server error", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutdown signal received...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	logger.Info("manager stopped")
}

func parseLogLevel(s string) (slog.Level, error) {
	var l slog.Level
	return l, l.UnmarshalText([]byte(s))
}
