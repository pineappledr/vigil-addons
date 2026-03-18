package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// envStr sets *dst to the value of the environment variable key if it is set.
func envStr(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

// envDuration parses a time.Duration from the environment variable key and
// sets *dst. Returns an error if the value is set but unparseable.
func envDuration(key string, dst *time.Duration) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", key, err)
	}
	*dst = d
	return nil
}

// envInt parses an integer from the environment variable key and sets *dst.
// If the value is set but unparseable, it is silently ignored.
func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}
