package hub

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// ReconnectConfig controls the exponential backoff for WebSocket reconnection.
type ReconnectConfig struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxAttempts  int // 0 means unlimited
}

// DefaultReconnectConfig provides sensible defaults for production use.
func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		InitialDelay: 1 * time.Second,
		MaxDelay:     60 * time.Second,
		MaxAttempts:  0,
	}
}

// ConnectFunc is called on each reconnect attempt. It should return nil on success.
type ConnectFunc func(ctx context.Context) error

// ReconnectLoop attempts to establish a connection using the provided function,
// retrying with exponential backoff on failure. It stops when ctx is cancelled
// or MaxAttempts is reached.
func ReconnectLoop(ctx context.Context, cfg ReconnectConfig, name string, connect ConnectFunc, logger *slog.Logger) {
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		logger.Info("connecting", "target", name, "attempt", attempt+1)
		err := connect(ctx)
		if err == nil {
			attempt = 0
			logger.Info("connection lost, will reconnect", "target", name)
			continue
		}

		if ctx.Err() != nil {
			return
		}

		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			logger.Error("max reconnect attempts reached", "target", name, "attempts", attempt, "error", err)
			return
		}

		delay := backoffDelay(cfg.InitialDelay, cfg.MaxDelay, attempt)
		logger.Warn("connection failed, retrying", "target", name, "attempt", attempt, "delay", delay, "error", err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func backoffDelay(initial, max time.Duration, attempt int) time.Duration {
	delay := time.Duration(float64(initial) * math.Pow(2, float64(attempt-1)))
	if delay > max {
		delay = max
	}
	return delay
}
