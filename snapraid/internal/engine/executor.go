package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
)

var ErrEngineLocked = errors.New("another snapraid operation is already running")

// Engine wraps the snapraid binary and enforces single-command concurrency.
type Engine struct {
	binaryPath string
	configPath string
	logger     *slog.Logger
	mu         sync.Mutex
}

// NewEngine creates an Engine with explicit binary and config paths.
func NewEngine(binaryPath, configPath string, logger *slog.Logger) *Engine {
	if binaryPath == "" {
		binaryPath = "snapraid"
	}
	return &Engine{
		binaryPath: binaryPath,
		configPath: configPath,
		logger:     logger,
	}
}

// commandResult holds captured output and exit code from a completed command.
type commandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runCommand executes the snapraid binary with the given arguments under mutex protection.
// It prepends --conf <configPath> automatically.
func (e *Engine) runCommand(ctx context.Context, args ...string) (*commandResult, error) {
	if !e.mu.TryLock() {
		return nil, ErrEngineLocked
	}
	defer e.mu.Unlock()

	fullArgs := append([]string{"--conf", e.configPath}, args...)
	e.logger.Info("executing snapraid", "args", fullArgs)

	cmd := exec.CommandContext(ctx, e.binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &commandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("snapraid exec: %w", err)
	}

	return result, nil
}

// lineFunc is called for each line of stdout during a streaming command.
type lineFunc func(line string)

// runCommandStreaming executes the snapraid binary and calls onLine for each stdout line in real-time.
// It is used by sync/scrub for progress tracking.
func (e *Engine) runCommandStreaming(ctx context.Context, onLine lineFunc, args ...string) (*commandResult, error) {
	if !e.mu.TryLock() {
		return nil, ErrEngineLocked
	}
	defer e.mu.Unlock()

	fullArgs := append([]string{"--conf", e.configPath}, args...)
	e.logger.Info("executing snapraid (streaming)", "args", fullArgs)

	cmd := exec.CommandContext(ctx, e.binaryPath, fullArgs...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("snapraid start: %w", err)
	}

	var stdoutBuf bytes.Buffer
	scanner := bufio.NewScanner(io.TeeReader(stdoutPipe, &stdoutBuf))
	for scanner.Scan() {
		line := scanner.Text()
		if onLine != nil {
			onLine(line)
		}
	}

	waitErr := cmd.Wait()

	result := &commandResult{
		Stdout: stdoutBuf.String(),
		Stderr: stderr.String(),
	}

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("snapraid exec: %w", waitErr)
	}

	return result, nil
}
