package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/agent"
	"github.com/pineapple/vigil-addons/burn-in/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.LoadAgentConfig()
	if err != nil {
		return err
	}

	// Resolve the advertise address: explicit config, or fall back to listen addr.
	advAddr := cfg.Agent.AdvertiseAddr
	if advAddr == "" {
		advAddr = cfg.Agent.Listen
	}

	hubClient := agent.NewHubClient(cfg.Hub.URL, cfg.Hub.PSK, cfg.Agent.ID, advAddr, logger)

	// Agent REST API with Ed25519 signature verification and job management.
	agentAPI, err := agent.NewAgentAPI(cfg.Agent.ServerPubkey, logger)
	if err != nil {
		return err
	}

	// Hub telemetry WebSocket client for streaming progress/log frames.
	hubTelemetry := agent.NewHubTelemetry(cfg.Hub.URL, cfg.Agent.ID, cfg.Hub.PSK, logger)

	httpServer := &http.Server{
		Addr:         cfg.Agent.Listen,
		Handler:      agentAPI.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Root context for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Register with the hub in the background.
	go func() {
		if err := hubClient.Register(ctx); err != nil {
			logger.Error("hub registration abandoned", "error", err)
		}
	}()

	// Maintain persistent telemetry WebSocket to the hub.
	go hubTelemetry.Run(ctx)

	// Start HTTP server.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("agent listening", "addr", cfg.Agent.Listen, "agent_id", cfg.Agent.ID)
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

	logger.Info("agent stopped")
	return nil
}
