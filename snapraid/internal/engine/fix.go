package engine

import (
	"context"
	"fmt"
)

// Fix executes `snapraid fix` with the given options.
func (e *Engine) Fix(ctx context.Context, opts FixOptions) (*FixReport, error) {
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

	result, err := e.runCommand(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("fix: %w", err)
	}

	return &FixReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
