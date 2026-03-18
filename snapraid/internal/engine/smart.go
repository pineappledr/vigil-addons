package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Smart executes `snapraid smart -v` and parses the output.
func (e *Engine) Smart(ctx context.Context) (*SmartReport, error) {
	result, err := e.runCommand(ctx, "smart", "-v")
	if err != nil {
		return nil, fmt.Errorf("smart: %w", err)
	}

	report := parseSmart(result.Stdout)
	report.Output = result.CombinedOutput()
	return report, nil
}

func parseSmart(output string) *SmartReport {
	r := &SmartReport{}

	for _, line := range strings.Split(output, "\n") {
		if m := reSmartDisk.FindStringSubmatch(line); m != nil {
			disk := SmartDisk{
				Temperature:        dashAtoi(m[1]),
				PowerOnDays:        dashAtoi(m[2]),
				ErrorCount:         dashAtoi(m[3]),
				FailureProbability: dashFloat(m[4]),
				Size:               parseSize(m[5], m[6]),
				Serial:             m[7],
				Device:             m[8],
				DiskName:           m[9],
				Status:             m[10],
			}
			r.Disks = append(r.Disks, disk)
		}
		if m := reOverallFail.FindStringSubmatch(line); m != nil {
			r.OverallFailProbability, _ = strconv.ParseFloat(m[1], 64)
		}
	}

	return r
}

func dashAtoi(s string) int {
	if s == "-" {
		return 0
	}
	return atoi(s)
}

func dashFloat(s string) float64 {
	if s == "-" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseSize(numStr, unit string) uint64 {
	f, _ := strconv.ParseFloat(numStr, 64)
	switch unit {
	case "TB":
		return uint64(f * 1_000_000_000_000)
	case "GB":
		return uint64(f * 1_000_000_000)
	case "MB":
		return uint64(f * 1_000_000)
	case "KB":
		return uint64(f * 1_000)
	default:
		return uint64(f)
	}
}
