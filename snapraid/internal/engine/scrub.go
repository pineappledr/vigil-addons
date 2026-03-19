package engine

import (
	"context"
	"fmt"
	"strconv"
)

// Scrub executes `snapraid scrub` with the given options.
// Progress updates (0-100) are sent to the progress channel if non-nil.
func (e *Engine) Scrub(ctx context.Context, opts ScrubOptions, progress chan<- int) (*ScrubReport, error) {
	args := []string{"scrub"}

	if opts.Plan != "" {
		args = append(args, "-p", opts.Plan)
	}
	if opts.OlderThanDays > 0 {
		args = append(args, "-o", strconv.Itoa(opts.OlderThanDays))
	}

	result, err := e.runCommandStreaming(ctx, progressLineFunc(progress), args...)
	if err != nil {
		return nil, fmt.Errorf("scrub: %w", err)
	}

	return &ScrubReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
