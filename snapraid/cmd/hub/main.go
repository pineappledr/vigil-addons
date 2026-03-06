package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	"github.com/pineappledr/vigil-addons/snapraid/internal/hub"
)

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

	registry, err := hub.NewRegistry(cfg.Data.RegistryPath)
	if err != nil {
		logger.Error("failed to initialize agent registry", "error", err)
		os.Exit(1)
	}

	upstreamCh := make(chan []byte, 256)
	aggregator := hub.NewAggregator(registry, upstreamCh, logger)
	router := hub.NewCommandRouter(registry, logger)

	srv := hub.NewServer(cfg, registry, aggregator, router, logger)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		logger.Error("server error", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
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
