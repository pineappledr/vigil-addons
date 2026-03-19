package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

const dockerTimeout = 2 * time.Minute

// manageContainers pauses/stops configured Docker containers before sync.
// Returns a restore function that unpauses/starts them after sync completes.
// Returns nil if no containers are configured.
func (p *Pipeline) manageContainers(ctx context.Context) func() {
	pause := p.cfg.Docker.PauseContainers
	stop := p.cfg.Docker.StopContainers

	if len(pause) == 0 && len(stop) == 0 {
		return nil
	}

	p.logger.Info("managing docker containers before sync",
		"pause", pause, "stop", stop)

	dockerCtx, cancel := context.WithTimeout(ctx, dockerTimeout)
	dockerBatch(dockerCtx, p.logger, "pause", pause)
	dockerBatch(dockerCtx, p.logger, "stop", stop)
	cancel()

	// Return a restore function.
	return func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), dockerTimeout)
		defer restoreCancel()

		p.logger.Info("restoring docker containers after sync")
		dockerBatch(restoreCtx, p.logger, "unpause", pause)
		dockerBatch(restoreCtx, p.logger, "start", stop)
	}
}

// dockerBatch runs a docker action against each container, logging success or failure.
func dockerBatch(ctx context.Context, logger *slog.Logger, action string, containers []string) {
	for _, name := range containers {
		if err := dockerCmd(ctx, action, name); err != nil {
			logger.Warn("docker "+action+" failed", "container", name, "error", err)
		} else {
			logger.Info("docker "+action+" succeeded", "container", name)
		}
	}
}

func dockerCmd(ctx context.Context, action, container string) error {
	cmd := exec.CommandContext(ctx, "docker", action, container)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s %s: %w (output: %s)", action, container, err, string(output))
	}
	return nil
}
