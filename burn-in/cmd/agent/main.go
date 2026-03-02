package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/agent"
	"github.com/pineapple/vigil-addons/burn-in/internal/config"
	"github.com/pineapple/vigil-addons/burn-in/internal/jobs"
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

	// Resolve the advertise address: explicit config, or auto-detect outbound IP.
	advAddr := cfg.Agent.AdvertiseAddr
	if advAddr == "" {
		_, port, _ := net.SplitHostPort(cfg.Agent.Listen)
		if port == "" {
			port = "9200"
		}
		if ip := detectOutboundIP(); ip != "" {
			advAddr = net.JoinHostPort(ip, port)
		} else {
			advAddr = cfg.Agent.Listen
		}
		logger.Info("auto-detected advertise address", "addr", advAddr)
	}

	hubClient := agent.NewHubClient(cfg.Hub.URL, cfg.Hub.PSK, cfg.Agent.ID, advAddr, logger)

	// Agent REST API with Ed25519 signature verification and job management.
	agentAPI, err := agent.NewAgentAPI(cfg.Agent.ServerPubkey, logger)
	if err != nil {
		return err
	}

	// Hub telemetry WebSocket client for streaming progress/log frames.
	hubTelemetry := agent.NewHubTelemetry(cfg.Hub.URL, cfg.Agent.ID, cfg.Hub.PSK, logger)

	// Job manager: dispatches burn-in/preclear/full jobs, streams telemetry.
	persist := jobs.NewJobPersistence("")
	jobManager := jobs.NewJobManager(hubTelemetry, persist, agentAPI, logger)
	agentAPI.SetJobDispatcher(&jobDispatcherAdapter{manager: jobManager})

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

// jobDispatcherAdapter bridges agent.JobDispatcher to jobs.JobManager,
// converting between the two packages' JobCommand types.
type jobDispatcherAdapter struct {
	manager *jobs.JobManager
}

func (a *jobDispatcherAdapter) StartJob(cmd agent.JobCommand) (string, error) {
	return a.manager.StartJob(jobs.JobCommand{
		AgentID: cmd.AgentID,
		Command: cmd.Command,
		Target:  cmd.Target,
		Params:  cmd.Params,
	})
}

// detectOutboundIP returns the preferred outbound IP by dialing a UDP socket.
// No actual traffic is sent.
func detectOutboundIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}
