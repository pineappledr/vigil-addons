package engine

import (
	"context"
	"fmt"
	"strconv"
)

// Scrub executes `snapraid scrub` with the given options.
func (e *Engine) Scrub(ctx context.Context, opts ScrubOptions) (*ScrubReport, error) {
	args := []string{"scrub"}

	if opts.Plan != "" {
		args = append(args, "-p", opts.Plan)
	}
	if opts.OlderThanDays > 0 {
		args = append(args, "-o", strconv.Itoa(opts.OlderThanDays))
	}

	result, err := e.runCommand(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("scrub: %w", err)
	}

	return &ScrubReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
