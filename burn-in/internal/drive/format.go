package drive

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// FormatProgress holds parsed state from a running mkfs process.
type FormatProgress struct {
	Phase   string  // Current formatting phase (e.g., "writing inode tables", "creating journal").
	Percent float64 // Estimated completion percentage.
}

// FormatResult holds the outcome of a formatting operation.
type FormatResult struct {
	Partition  string `json:"partition"`
	Filesystem string `json:"filesystem"`
	ReservedPct int   `json:"reserved_pct"`
}

// FormatCallback is invoked on each progress update parsed from mkfs output.
type FormatCallback func(progress FormatProgress)

// FormatPartition formats the given partition with the specified filesystem.
// For "raw", no formatting is performed (the partition is left unformatted
// for ZFS pool creation). Progress is streamed via the callback.
func FormatPartition(ctx context.Context, partition, fileSystem string, reservedPct int, onProgress FormatCallback, logger *slog.Logger) (*FormatResult, error) {
	if !isValidDevicePath(partition) {
		return nil, fmt.Errorf("invalid partition path: %q", partition)
	}

	switch fileSystem {
	case "ext4":
		return formatExt4(ctx, partition, reservedPct, onProgress)
	case "xfs":
		return formatSimple(ctx, partition, "xfs", "mkfs.xfs", "-f", partition)
	case "btrfs":
		return formatSimple(ctx, partition, "btrfs", "mkfs.btrfs", "-f", partition)
	case "fat32":
		return formatSimple(ctx, partition, "fat32", "mkfs.fat", "-F", "32", partition)
	case "linux-swap":
		return formatSimple(ctx, partition, "linux-swap", "mkswap", partition)
	case "raw":
		logger.Info("formatting skipped for raw/ZFS partition", "partition", partition)
		return &FormatResult{
			Partition:  partition,
			Filesystem: "raw",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported filesystem: %q", fileSystem)
	}
}

// formatSimple runs a single mkfs command without progress parsing.
func formatSimple(ctx context.Context, partition, fsName string, cmdName string, args ...string) (*FormatResult, error) {
	result, err := runWithProcessGroup(ctx, cmdName, args...)
	if err != nil {
		// Combine stdout+stderr for the error message since mkfs tools vary.
		combined := strings.TrimSpace(string(result.Stdout) + "\n" + string(result.Stderr))
		if combined != "" {
			return nil, fmt.Errorf("%s failed: %s", cmdName, combined)
		}
		return nil, fmt.Errorf("%s failed: %w", cmdName, err)
	}

	return &FormatResult{
		Partition:  partition,
		Filesystem: fsName,
	}, nil
}

// mkfs progress regex matches lines like:
//
//	"Writing inode tables:  12/125  done"
//	"Writing inode tables: done"
var mkfsProgressRegex = regexp.MustCompile(
	`(Writing inode tables|Creating journal|Writing superblocks).*?(\d+)/(\d+)`,
)

// formatExt4 formats the given partition with ext4 using the specified
// reserved block percentage. It streams progress via the callback.
func formatExt4(ctx context.Context, partition string, reservedPct int, onProgress FormatCallback) (*FormatResult, error) {
	if reservedPct < 0 || reservedPct > 50 {
		return nil, fmt.Errorf("reserved percentage must be 0-50, got %d", reservedPct)
	}

	args := []string{
		"-F",
		"-T", "largefile4",
		"-m", strconv.Itoa(reservedPct),
		partition,
	}

	cmd := exec.CommandContext(ctx, "mkfs.ext4", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// mkfs.ext4 writes progress to stdout.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	// Capture stderr for error messages.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting mkfs.ext4: %w", err)
	}

	// Kill entire process group on context cancellation.
	cleanup := killProcessGroupOnCancel(ctx, cmd)
	defer cleanup()

	// Parse stdout for progress in a goroutine.
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		parseMkfsOutput(stdoutPipe, onProgress)
	}()

	// Drain stderr.
	stderrDone := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(stderrPipe)
		stderrDone <- strings.TrimSpace(string(data))
	}()

	<-progressDone
	stderrMsg := <-stderrDone

	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if stderrMsg != "" {
			return nil, fmt.Errorf("mkfs.ext4 failed: %s", stderrMsg)
		}
		return nil, fmt.Errorf("mkfs.ext4 failed: %w", err)
	}

	return &FormatResult{
		Partition:   partition,
		Filesystem:  "ext4",
		ReservedPct: reservedPct,
	}, nil
}

// parseMkfsOutput reads mkfs.ext4 stdout and emits progress callbacks.
// mkfs.ext4 outputs phases sequentially:
//  1. Writing inode tables (bulk of the work)
//  2. Creating journal
//  3. Writing superblocks and filesystem accounting information
func parseMkfsOutput(r io.Reader, onProgress FormatCallback) {
	if onProgress == nil {
		io.ReadAll(r)
		return
	}

	// mkfs.ext4 uses \r for in-place progress updates and \n between phases.
	scanner := bufio.NewScanner(r)
	scanner.Split(splitOnCRLF)

	// Phase weights for overall progress estimation:
	// inode tables ~70%, journal ~15%, superblocks ~15%.
	phaseWeights := map[string]struct {
		base   float64
		weight float64
	}{
		"Writing inode tables": {0, 70},
		"Creating journal":     {70, 15},
		"Writing superblocks":  {85, 15},
	}

	for scanner.Scan() {
		line := scanner.Text()

		m := mkfsProgressRegex.FindStringSubmatch(line)
		if m == nil {
			// Check for phase completion lines without numeric progress.
			for phase, pw := range phaseWeights {
				if strings.Contains(line, phase) && strings.Contains(line, "done") {
					onProgress(FormatProgress{
						Phase:   phase,
						Percent: pw.base + pw.weight,
					})
				}
			}
			continue
		}

		phaseName := m[1]
		current, _ := strconv.ParseFloat(m[2], 64)
		total, _ := strconv.ParseFloat(m[3], 64)

		if total <= 0 {
			continue
		}

		phasePercent := current / total * 100.0

		pw, ok := phaseWeights[phaseName]
		if !ok {
			pw = struct {
				base   float64
				weight float64
			}{0, 100}
		}

		overallPercent := pw.base + (phasePercent/100.0)*pw.weight

		onProgress(FormatProgress{
			Phase:   phaseName,
			Percent: overallPercent,
		})
	}
}
