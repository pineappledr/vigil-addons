package scheduler

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

const dockerTimeout = 2 * time.Minute

// manageContainers pauses/stops configured Docker containers before sync.
// Returns a restore function that unpauses/starts them after sync completes.
// Returns nil if no containers are configured.
func (p *Pipeline) manageContainers(ctx context.Context, doPause bool) func() {
	pause := p.cfg.Docker.PauseContainers
	stop := p.cfg.Docker.StopContainers

	if len(pause) == 0 && len(stop) == 0 {
		return nil
	}

	p.logger.Info("managing docker containers before sync",
		"pause", pause, "stop", stop)

	dockerCtx, cancel := context.WithTimeout(ctx, dockerTimeout)

	for _, name := range pause {
		if err := dockerCmd(dockerCtx, "pause", name); err != nil {
			p.logger.Warn("failed to pause container", "container", name, "error", err)
		} else {
			p.logger.Info("paused container", "container", name)
		}
	}

	for _, name := range stop {
		if err := dockerCmd(dockerCtx, "stop", name); err != nil {
			p.logger.Warn("failed to stop container", "container", name, "error", err)
		} else {
			p.logger.Info("stopped container", "container", name)
		}
	}

	cancel()

	// Return a restore function.
	return func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), dockerTimeout)
		defer restoreCancel()

		p.logger.Info("restoring docker containers after sync")

		for _, name := range pause {
			if err := dockerCmd(restoreCtx, "unpause", name); err != nil {
				p.logger.Warn("failed to unpause container", "container", name, "error", err)
			} else {
				p.logger.Info("unpaused container", "container", name)
			}
		}

		for _, name := range stop {
			if err := dockerCmd(restoreCtx, "start", name); err != nil {
				p.logger.Warn("failed to start container", "container", name, "error", err)
			} else {
				p.logger.Info("started container", "container", name)
			}
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
