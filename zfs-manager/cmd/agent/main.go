package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineappledr/vigil-addons/zfs-manager/internal/agent"
	"github.com/pineappledr/vigil-addons/zfs-manager/internal/config"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.agent.yaml", "path to agent configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.LoadAgentConfig(*configPath)
	if err != nil {
		logger.Error("failed to load agent config", "error", err)
		os.Exit(1)
	}

	if level, err := parseLogLevel(cfg.Logging.Level); err == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}

	config.LogAgentConfig(logger, cfg)

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	engine := agent.NewEngine(cfg.ZFS.ZpoolPath, cfg.ZFS.ZfsPath, logger)

	hostname, _ := os.Hostname()
	agentID := cfg.Identity.AgentID
	if agentID == "" {
		agentID = hostname
	}

	collector := agent.NewCollector(agentID, hostname, engine, logger)

	// Start periodic telemetry collection (every 60s).
	go collector.StartCollectionLoop(appCtx, 60*time.Second)

	srv := agent.NewServer(cfg, engine, collector, logger)

	// Self-register with the Hub (retries in background).
	if cfg.Hub.URL != "" {
		advertiseAddr := cfg.Identity.AdvertiseAddr
		if advertiseAddr == "" {
			logger.Error("VIGIL_ZFS_AGENT_ADVERTISE_ADDR is required for Hub registration")
		} else {
			go agent.RegisterWithHub(appCtx, cfg.Hub.URL, cfg.Hub.PSK, agentID, hostname, advertiseAddr, version, logger)
			go agent.StartHubForwarder(appCtx, collector, cfg.Hub.URL, cfg.Hub.PSK, agentID, 30*time.Second, logger)
		}
	}

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

	appCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	logger.Info("agent stopped")
}

func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))
	return level, err
}
