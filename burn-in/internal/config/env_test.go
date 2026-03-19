package config

import (
	"testing"
	"time"
)

func TestEnvStr(t *testing.T) {
	t.Setenv("TEST_ENV_STR", "hello")

	var dst string
	envStr("TEST_ENV_STR", &dst)
	if dst != "hello" {
		t.Errorf("envStr: got %q, want %q", dst, "hello")
	}
}

func TestEnvStr_Unset(t *testing.T) {
	dst := "default"
	envStr("TEST_ENV_STR_MISSING", &dst)
	if dst != "default" {
		t.Errorf("envStr unset: got %q, want %q", dst, "default")
	}
}

func TestEnvDuration(t *testing.T) {
	t.Setenv("TEST_ENV_DUR", "5s")

	var dst time.Duration
	if err := envDuration("TEST_ENV_DUR", &dst); err != nil {
		t.Fatal(err)
	}
	if dst != 5*time.Second {
		t.Errorf("envDuration: got %v, want %v", dst, 5*time.Second)
	}
}

func TestEnvDuration_Invalid(t *testing.T) {
	t.Setenv("TEST_ENV_DUR_BAD", "notaduration")

	var dst time.Duration
	if err := envDuration("TEST_ENV_DUR_BAD", &dst); err == nil {
		t.Error("envDuration: expected error for invalid duration")
	}
}

func TestEnvDuration_Unset(t *testing.T) {
	dst := 10 * time.Second
	if err := envDuration("TEST_ENV_DUR_MISSING", &dst); err != nil {
		t.Fatal(err)
	}
	if dst != 10*time.Second {
		t.Errorf("envDuration unset: got %v, want %v", dst, 10*time.Second)
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("TEST_ENV_INT", "42")

	var dst int
	envInt("TEST_ENV_INT", &dst)
	if dst != 42 {
		t.Errorf("envInt: got %d, want %d", dst, 42)
	}
}

func TestEnvInt_Invalid(t *testing.T) {
	t.Setenv("TEST_ENV_INT_BAD", "abc")

	dst := 99
	envInt("TEST_ENV_INT_BAD", &dst)
	if dst != 99 {
		t.Errorf("envInt invalid: got %d, want %d (unchanged)", dst, 99)
	}
}

func TestEnvInt_Unset(t *testing.T) {
	dst := 7
	envInt("TEST_ENV_INT_MISSING", &dst)
	if dst != 7 {
		t.Errorf("envInt unset: got %d, want %d", dst, 7)
	}
}
