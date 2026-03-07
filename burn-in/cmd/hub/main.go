package main

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/config"
	"github.com/pineapple/vigil-addons/burn-in/internal/hub"
)

//go:embed manifest.json
var manifestFS embed.FS

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.LoadHubConfig()
	if err != nil {
		return err
	}

	registry, err := hub.NewAgentRegistry(cfg.Hub.DataDir, logger)
	if err != nil {
		return err
	}

	// Read the embedded manifest.
	manifestData, err := manifestFS.ReadFile("manifest.json")
	if err != nil {
		return err
	}

	// Create Vigil registration client.
	vigilClient, err := hub.NewVigilClient(cfg.Vigil.URL, cfg.Vigil.AgentToken, manifestData, logger)
	if err != nil {
		return err
	}

	// Create the aggregator (upstream telemetry client is set after registration).
	aggregator := hub.NewAggregator(nil, registry, cfg.Hub.AgentPSK, hub.AlertThresholds{
		TempWarningC:  cfg.Alerts.TempWarningC,
		TempCriticalC: cfg.Alerts.TempCriticalC,
	}, logger)

	srv := hub.NewServer(registry, aggregator, cfg.Hub.AgentPSK, cfg.Hub.AdvertiseURL, cfg.Hub.DataDir, logger)

	httpServer := &http.Server{
		Addr:         cfg.Hub.Listen,
		Handler:      srv.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Root context for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Register with the Vigil server in the background, then start telemetry.
	go func() {
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
			cfg.Vigil.URL,
			resp.AddonID,
			cfg.Vigil.AgentToken,
			cfg.Hub.HeartbeatInterval,
			logger,
		)

		// Start the WS connection in the background; wait for it to
		// become ready before wiring it into the aggregator so that
		// no frames are dropped during the initial dial.
		go telemetry.Run(ctx)

		select {
		case <-telemetry.Ready():
			logger.Info("upstream telemetry pipeline ready, enabling aggregator relay")
			aggregator.SetUpstream(telemetry)
		case <-ctx.Done():
			return
		}
	}()

	// Start HTTP server.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("hub listening", "addr", cfg.Hub.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal or server error.
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("hub stopped")
	return nil
}
