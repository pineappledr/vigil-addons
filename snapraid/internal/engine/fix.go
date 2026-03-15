package engine

import (
	"context"
	"fmt"
	"strconv"
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

	lastPct := -1
	onLine := func(line string) {
		if progress == nil {
			return
		}
		if m := reProgress.FindStringSubmatch(line); m != nil {
			pct, err := strconv.Atoi(m[1])
			if err != nil {
				return
			}
			if pct != lastPct {
				lastPct = pct
				select {
				case progress <- pct:
				default:
				}
			}
		}
	}

	result, err := e.runCommandStreaming(ctx, onLine, args...)
	if err != nil {
		return nil, fmt.Errorf("fix: %w", err)
	}

	return &FixReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
