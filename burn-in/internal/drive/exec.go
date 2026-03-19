package drive

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// commandResult holds captured output from a completed command.
type commandResult struct {
	Stdout []byte
	Stderr []byte
}

// runWithProcessGroup executes a command in its own process group so the
// entire tree can be killed on context cancellation. Returns captured
// stdout/stderr and any execution error. If the context is cancelled,
// the context error is returned directly.
func runWithProcessGroup(ctx context.Context, name string, args ...string) (*commandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	cleanup := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	defer cleanup()

	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return &commandResult{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}, err
}

// killProcessGroupOnCancel registers a context.AfterFunc that sends SIGKILL
// to the process group of cmd when ctx is cancelled. Use this for streaming
// commands where the caller manages pipes and cmd.Start/Wait directly.
// The returned function must be deferred to unregister the callback.
func killProcessGroupOnCancel(ctx context.Context, cmd *exec.Cmd) func() bool {
	return context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
}

// stderrMessage returns the trimmed stderr content, or falls back to the
// given error if stderr is empty. Useful for building error messages from
// external tool failures.
func stderrMessage(result *commandResult, fallbackErr error, prefix string) error {
	msg := strings.TrimSpace(string(result.Stderr))
	if msg != "" {
		return fmt.Errorf("%s: %s", prefix, msg)
	}
	return fmt.Errorf("%s: %w", prefix, fallbackErr)
}

// splitOnCRLF is a bufio.SplitFunc that splits on \r or \n, handling the
// carriage-return progress updates from badblocks and mkfs.
func splitOnCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	for i := 0; i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			// Skip consecutive delimiters.
			j := i + 1
			for j < len(data) && (data[j] == '\n' || data[j] == '\r') {
				j++
			}
			return j, data[:i], nil
		}
	}

	if atEOF {
		return len(data), data, nil
	}

	return 0, nil, nil
}
