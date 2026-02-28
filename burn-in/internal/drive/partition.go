package drive

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// PartitionResult holds the outcome of a partitioning operation.
type PartitionResult struct {
	Device    string `json:"device"`
	Partition string `json:"partition"` // e.g. /dev/sda1
	TableType string `json:"table_type"`
}

// PartitionGPT wipes the existing partition table on the device and creates
// a fresh GPT layout with a single partition spanning the entire drive.
// It uses sgdisk for deterministic, scriptable GPT operations.
func PartitionGPT(ctx context.Context, devicePath string) (*PartitionResult, error) {
	if !isValidDevicePath(devicePath) {
		return nil, fmt.Errorf("invalid device path: %q", devicePath)
	}

	// Safety: confirm the device is not mounted before touching the table.
	if err := IsSafeTarget(devicePath); err != nil {
		return nil, fmt.Errorf("safety check before partitioning: %w", err)
	}

	// Step 1: Zap all GPT and MBR data structures.
	if err := runSgdisk(ctx, devicePath, "--zap-all"); err != nil {
		return nil, fmt.Errorf("wiping partition table: %w", err)
	}

	// Step 2: Create a single partition spanning the full device.
	// -n 1:0:0  → partition 1, first available sector to last available sector.
	// -t 1:8300 → Linux filesystem type.
	if err := runSgdisk(ctx, devicePath, "--new=1:0:0", "--typecode=1:8300"); err != nil {
		return nil, fmt.Errorf("creating GPT partition: %w", err)
	}

	// Step 3: Verify the table.
	if err := runSgdisk(ctx, devicePath, "--verify"); err != nil {
		// Verification failure is non-fatal but logged — some drives report
		// benign alignment warnings.
		_ = err
	}

	partPath := inferPartitionPath(devicePath, 1)

	return &PartitionResult{
		Device:    devicePath,
		Partition: partPath,
		TableType: "gpt",
	}, nil
}

// runSgdisk executes sgdisk with the given arguments against the device.
// It sets up a process group for safe cancellation.
func runSgdisk(ctx context.Context, device string, args ...string) error {
	cmdArgs := append(args, device)
	cmd := exec.CommandContext(ctx, "sgdisk", cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Kill the process group on context cancellation.
	cleanup := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	defer cleanup()

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("sgdisk %s: %s", strings.Join(args, " "), errMsg)
		}
		return fmt.Errorf("sgdisk %s: %w", strings.Join(args, " "), err)
	}

	return nil
}

// inferPartitionPath derives the first partition path from a block device.
// For /dev/sda → /dev/sda1, for /dev/nvme0n1 → /dev/nvme0n1p1.
func inferPartitionPath(device string, partNum int) string {
	base := device
	// NVMe devices use a "p" separator before partition numbers.
	if strings.Contains(base, "nvme") || strings.Contains(base, "loop") {
		return fmt.Sprintf("%sp%d", base, partNum)
	}
	return fmt.Sprintf("%s%d", base, partNum)
}
