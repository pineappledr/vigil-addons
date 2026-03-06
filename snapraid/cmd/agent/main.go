package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineappledr/vigil-addons/snapraid/internal/agent"
	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	agentdb "github.com/pineappledr/vigil-addons/snapraid/internal/db"
	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.agent.yaml", "path to agent configuration file")
	dbPath := flag.String("db", "/var/lib/vigil-snapraid-agent/agent.db", "path to agent SQLite database")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.LoadAgentConfig(*configPath)
	if err != nil {
		logger.Error("failed to load agent config", "error", err)
		os.Exit(1)
	}

	if level, err := parseLogLevel(cfg.Logging.Level); err == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		}))
	}

	db, err := agentdb.Open(*dbPath)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	config.LogAgentConfig(logger, cfg)

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	agentdb.StartPruneLoop(appCtx, db, 90*24*time.Hour, logger)

	eng := engine.NewEngine(cfg.SnapRAID.BinaryPath, cfg.SnapRAID.ConfigPath, logger)

	hostname, _ := os.Hostname()
	collector := agent.NewCollector(hostname, hostname, version, logger)

	srv := agent.NewServer(cfg, eng, db, collector, logger)

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
