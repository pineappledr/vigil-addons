package drive

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// BadblocksProgress holds the parsed state of a running badblocks process.
type BadblocksProgress struct {
	Pattern      int     // Current write pattern (1-4).
	PatternHex   string  // Hex value of the pattern (e.g., "0xaa").
	BlocksDone   int64   // Number of blocks processed in current pass.
	BlocksTotal  int64   // Total number of blocks on the device.
	Percent      float64 // Overall completion percentage across all 4 patterns.
	Errors       int     // Cumulative bad block count.
	Phase        string  // Human-readable phase (e.g., "writing pattern 1/4").
}

// BadblocksResult holds the final outcome of a badblocks run.
type BadblocksResult struct {
	Errors  int    // Total bad blocks found.
	LogFile string // Path to the badblocks output log file.
	Aborted bool   // True if the run was cancelled.
}

// BadblocksCallback is invoked each time a progress update is parsed from
// the badblocks process output.
type BadblocksCallback func(progress BadblocksProgress)

// pattern order used by badblocks -w (destructive write mode).
var patternOrder = []string{"0xaa", "0x55", "0xff", "0x00"}

// progressRegex matches badblocks progress lines on stderr:
//   "Reading and comparing:  12.34% done, 0:05 elapsed. (0/0/0 errors)"
//   "Writing pattern 0xaa:  50.00% done, 1:23 elapsed. (0/0/0 errors)"
var progressRegex = regexp.MustCompile(
	`(?:Writing pattern (0x[0-9a-fA-F]+)|Reading and comparing):\s+(\d+\.?\d*)% done.*?\((\d+)/(\d+)/(\d+) errors\)`,
)

// blockCountRegex matches the initial line reporting total block count.
var blockCountRegex = regexp.MustCompile(`Checking blocks (\d+) to (\d+)`)

// RunBadblocks executes a destructive badblocks test on the given device.
// It streams progress via the callback and respects context cancellation by
// killing the entire process group.
func RunBadblocks(ctx context.Context, device string, blockSize, concurrentBlocks int, abortOnError bool, logDir string, onProgress BadblocksCallback) (*BadblocksResult, error) {
	if !isValidDevicePath(device) {
		return nil, fmt.Errorf("invalid device path: %q", device)
	}

	if blockSize <= 0 {
		blockSize = 4096
	}
	if concurrentBlocks <= 0 {
		concurrentBlocks = 65536
	}

	logFile := filepath.Join(logDir, fmt.Sprintf("badblocks-%s.log", SafeDeviceName(device)))

	maxErrors := "0" // unlimited
	if abortOnError {
		maxErrors = "1"
	}

	args := []string{
		"-b", strconv.Itoa(blockSize),
		"-c", strconv.Itoa(concurrentBlocks),
		"-wsv",
		"-e", maxErrors,
		"-o", logFile,
		device,
	}

	cmd := exec.CommandContext(ctx, "badblocks", args...)

	// Create a new process group so we can kill badblocks and all children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// badblocks writes progress to stderr.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	// stdout captures error block numbers.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting badblocks: %w", err)
	}

	result := &BadblocksResult{LogFile: logFile}

	// Kill the entire process group on context cancellation.
	cleanup := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	defer cleanup()

	var wg sync.WaitGroup

	// Parse stderr for progress.
	wg.Add(1)
	go func() {
		defer wg.Done()
		parseStderr(stderrPipe, onProgress, result)
	}()

	// Count error blocks from stdout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		countStdoutErrors(stdoutPipe, result)
	}()

	wg.Wait()

	err = cmd.Wait()
	if ctx.Err() != nil {
		result.Aborted = true
		return result, ctx.Err()
	}
	if err != nil {
		// badblocks exits non-zero when it finds errors.
		if result.Errors > 0 {
			return result, nil
		}
		return result, fmt.Errorf("badblocks exited: %w", err)
	}

	return result, nil
}

// parseStderr reads badblocks stderr and emits progress callbacks.
func parseStderr(r io.Reader, onProgress BadblocksCallback, result *BadblocksResult) {
	scanner := bufio.NewScanner(r)
	// badblocks uses \r for progress updates within a single line.
	scanner.Split(splitOnCRLF)

	var totalBlocks int64
	currentPattern := 0

	for scanner.Scan() {
		line := scanner.Text()

		// Detect total block count from the initial header.
		if m := blockCountRegex.FindStringSubmatch(line); len(m) == 3 {
			start, _ := strconv.ParseInt(m[1], 10, 64)
			end, _ := strconv.ParseInt(m[2], 10, 64)
			totalBlocks = end - start + 1
			continue
		}

		m := progressRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		patternHex := m[1]
		passPercent, _ := strconv.ParseFloat(m[2], 64)
		readErrs, _ := strconv.Atoi(m[3])
		writeErrs, _ := strconv.Atoi(m[4])
		corruptErrs, _ := strconv.Atoi(m[5])
		totalErrs := readErrs + writeErrs + corruptErrs

		result.Errors = totalErrs

		// Determine which pattern we are on.
		if patternHex != "" {
			for i, p := range patternOrder {
				if strings.EqualFold(p, patternHex) {
					currentPattern = i + 1
					break
				}
			}
		} else {
			// "Reading and comparing" is the verification pass for the current pattern.
			// It comes right after the write pass, so currentPattern stays the same.
		}

		if currentPattern == 0 {
			currentPattern = 1
		}

		// Each pattern has a write pass + read/compare pass.
		// 4 patterns = 8 passes total. Each write or read pass is 1/8 of overall.
		// Pattern N write = pass (N-1)*2, read = pass (N-1)*2+1.
		isReadPass := patternHex == ""
		passIndex := (currentPattern-1)*2
		if isReadPass {
			passIndex++
		}
		overallPercent := (float64(passIndex) + passPercent/100.0) / 8.0 * 100.0

		if onProgress != nil {
			onProgress(BadblocksProgress{
				Pattern:     currentPattern,
				PatternHex:  patternOrder[currentPattern-1],
				BlocksDone:  int64(passPercent / 100.0 * float64(totalBlocks)),
				BlocksTotal: totalBlocks,
				Percent:     overallPercent,
				Errors:      totalErrs,
				Phase:       fmt.Sprintf("pattern %d/4", currentPattern),
			})
		}
	}
}

// countStdoutErrors counts the bad block numbers printed to stdout.
func countStdoutErrors(r io.Reader, result *BadblocksResult) {
	scanner := bufio.NewScanner(r)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			count++
		}
	}
	if count > result.Errors {
		result.Errors = count
	}
}


// SafeDeviceName converts a device path like /dev/sda to a safe filename component.
func SafeDeviceName(device string) string {
	name := filepath.Base(device)
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, name)
}

// EnsureLogDir creates the log directory for badblocks output if it does not exist.
func EnsureLogDir(dir string) error {
	return os.MkdirAll(filepath.Clean(dir), 0o750)
}
