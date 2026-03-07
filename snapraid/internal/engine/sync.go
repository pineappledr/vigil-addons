package engine

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

var reSyncProgress = regexp.MustCompile(`(\d+)%`)

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

	lastPct := -1
	onLine := func(line string) {
		if progress == nil {
			return
		}
		if m := reSyncProgress.FindStringSubmatch(line); m != nil {
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
		return nil, fmt.Errorf("sync: %w", err)
	}

	return &SyncReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
