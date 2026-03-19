package engine

import (
	"context"
	"fmt"
)

// Sync executes `snapraid sync` with the given options.
// Progress updates (0-100) are sent to the progress channel if non-nil.
// The channel is never closed by this method; the caller manages its lifecycle.
func (e *Engine) Sync(ctx context.Context, opts SyncOptions, progress chan<- int) (*SyncReport, error) {
	args := []string{"sync"}

	if opts.PreHash {
		args = append(args, "--pre-hash")
	}
	if opts.ForceZero {
		args = append(args, "--force-zero")
	}
	if opts.ForceEmpty {
		args = append(args, "--force-empty")
	}
	if opts.ForceFull {
		args = append(args, "--force-full")
	}

	result, err := e.runCommandStreaming(ctx, progressLineFunc(progress), args...)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}

	return &SyncReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
