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
	agentdb "github.com/pineappledr/vigil-addons/zfs-manager/internal/db"
	"github.com/pineappledr/vigil-addons/zfs-manager/internal/scheduler"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.agent.yaml", "path to agent configuration file")
	dbPath := flag.String("db", "/data/agent.db", "path to agent SQLite database")
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

	// Open SQLite database for scheduled tasks and job history.
	db, err := agentdb.Open(*dbPath)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	// Start daily prune of old job history (30 day retention).
	agentdb.StartPruneLoop(appCtx, db, 30*24*time.Hour, logger)

	engine := agent.NewEngine(cfg.ZFS.ZpoolPath, cfg.ZFS.ZfsPath, logger)

	hostname, _ := os.Hostname()
	agentID := cfg.Identity.AgentID
	if agentID == "" {
		agentID = hostname
	}

	collector := agent.NewCollector(agentID, hostname, engine, logger)

	// Start periodic telemetry collection (every 60s).
	go collector.StartCollectionLoop(appCtx, 60*time.Second)

	// Start the cron scheduler for periodic snapshot and scrub tasks.
	sched := scheduler.New(engine, collector, db, logger)
	if err := sched.Start(appCtx); err != nil {
		logger.Error("failed to start scheduler", "error", err)
		os.Exit(1)
	}
	defer sched.Stop()

	srv := agent.NewServer(cfg, engine, collector, db, sched, logger)

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
