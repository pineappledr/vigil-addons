package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

const hookTimeout = 5 * time.Minute

// runHook executes a shell command string as a pre/post hook.
// Returns nil if the hook is empty (not configured).
func runHook(ctx context.Context, name, command string, logger *slog.Logger) error {
	if command == "" {
		return nil
	}

	logger.Info("running hook", "hook", name, "command", command)

	hookCtx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "sh", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %s failed: %w (output: %s)", name, err, string(output))
	}

	logger.Info("hook completed", "hook", name, "output", string(output))
	return nil
}
