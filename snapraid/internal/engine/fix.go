package engine

import (
	"context"
	"fmt"
)

// Fix executes `snapraid fix` with the given options.
// Progress updates (0-100) are sent to the progress channel if non-nil.
func (e *Engine) Fix(ctx context.Context, opts FixOptions, progress chan<- int) (*FixReport, error) {
	args := []string{"fix"}

	if opts.BadBlocksOnly {
		args = append(args, "-e")
	}
	if opts.Disk != "" {
		args = append(args, "-d", opts.Disk)
	}
	if opts.Filter != "" {
		args = append(args, "-f", opts.Filter)
	}

	result, err := e.runCommandStreaming(ctx, progressLineFunc(progress), args...)
	if err != nil {
		return nil, fmt.Errorf("fix: %w", err)
	}

	return &FixReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
