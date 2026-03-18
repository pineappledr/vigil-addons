package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Status executes `snapraid status` and parses the output.
func (e *Engine) Status(ctx context.Context) (*StatusReport, error) {
	result, err := e.runCommand(ctx, "status")
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}

	report, err := parseStatus(result.Stdout)
	if err != nil {
		return nil, fmt.Errorf("status parse: %w", err)
	}

	report.Output = result.CombinedOutput()
	return report, nil
}

func parseStatus(output string) (*StatusReport, error) {
	r := &StatusReport{}
	now := time.Now().UTC()

	for _, line := range strings.Split(output, "\n") {
		if m := reFiles.FindStringSubmatch(line); m != nil {
			r.Files = atoi(m[1])
		}
		if m := reFragmented.FindStringSubmatch(line); m != nil {
			r.Fragmented = atoi(m[1])
		}
		if m := reExcessFragments.FindStringSubmatch(line); m != nil {
			r.ExcessFragments = atoi(m[1])
		}
		if m := reFileSize.FindStringSubmatch(line); m != nil {
			r.FileSize = atoU64(m[1]) * 1_000_000_000
		}
		if m := reParitySize.FindStringSubmatch(line); m != nil {
			r.ParitySize = atoU64(m[1]) * 1_000_000_000
		}
		if m := reWastedSpace.FindStringSubmatch(line); m != nil {
			r.WastedSpace = atoU64(m[1]) * 1_000_000_000
		}
		if m := reUnsyncedBlocks.FindStringSubmatch(line); m != nil {
			r.UnsyncedBlocks = atoi(m[1])
		}
		if m := reUnscrubbedPct.FindStringSubmatch(line); m != nil {
			r.UnscrubbedPercent, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := reBadBlocks.FindStringSubmatch(line); m != nil {
			r.BadBlocks = atoi(m[1])
		}
		if m := reScrubOldest.FindStringSubmatch(line); m != nil {
			r.ScrubAge.Oldest = now.AddDate(0, 0, -atoi(m[1]))
		}
		if m := reScrubMedian.FindStringSubmatch(line); m != nil {
			r.ScrubAge.Median = now.AddDate(0, 0, -atoi(m[1]))
		}
		if m := reScrubNewest.FindStringSubmatch(line); m != nil {
			r.ScrubAge.Newest = now.AddDate(0, 0, -atoi(m[1]))
		}
		if m := reDiskLine.FindStringSubmatch(line); m != nil {
			r.DiskStatus = append(r.DiskStatus, DiskInfo{
				Name:   m[2],
				Device: m[3],
				Used:   atoU64(m[5]) * 1_000_000_000,
				Free:   atoU64(m[6]) * 1_000_000_000,
				Wasted: atoU64(m[7]) * 1_000_000_000,
			})
		}
	}

	return r, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atoU64(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}
