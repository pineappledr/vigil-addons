package drive

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// VerifyResult holds the outcome of a filesystem verification.
type VerifyResult struct {
	Partition   string       `json:"partition"`
	MountOK     bool         `json:"mount_ok"`
	MetadataOK  bool         `json:"metadata_ok"`
	SmartDelta  *SmartDelta  `json:"smart_delta,omitempty"`
	SmartOK     bool         `json:"smart_ok"`
}

// VerifyFilesystem mounts the partition read-only, runs a metadata check via
// dumpe2fs, then unmounts and takes a post-format SMART snapshot to detect
// any hardware degradation caused by the formatting process.
func VerifyFilesystem(ctx context.Context, partition string, baseline *SmartSnapshot) (*VerifyResult, error) {
	if !isValidDevicePath(partition) {
		return nil, fmt.Errorf("invalid partition path: %q", partition)
	}

	result := &VerifyResult{
		Partition: partition,
	}

	// Step 1: Mount read-only to a temporary directory.
	mountDir, err := os.MkdirTemp("", "burnin-verify-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp mount dir: %w", err)
	}
	defer os.Remove(mountDir)

	if err := mountReadOnly(ctx, partition, mountDir); err != nil {
		result.MountOK = false
		return result, fmt.Errorf("read-only mount failed: %w", err)
	}
	result.MountOK = true

	// Always unmount, even on error.
	defer unmount(mountDir)

	// Step 2: Verify filesystem metadata via dumpe2fs.
	if err := checkMetadata(ctx, partition); err != nil {
		result.MetadataOK = false
		return result, fmt.Errorf("metadata check failed: %w", err)
	}
	result.MetadataOK = true

	// Unmount before SMART snapshot to avoid any I/O interference.
	if err := unmount(mountDir); err != nil {
		return result, fmt.Errorf("unmount failed: %w", err)
	}

	// Step 3: Post-format SMART snapshot and delta check.
	if baseline != nil {
		devicePath := inferDeviceFromPartition(partition)
		postSnap, err := TakeSnapshot(devicePath)
		if err != nil {
			result.SmartOK = false
			return result, fmt.Errorf("post-format SMART snapshot failed: %w", err)
		}

		delta := ComputeDelta(baseline, postSnap)
		result.SmartDelta = delta
		result.SmartOK = !delta.Degraded
	} else {
		result.SmartOK = true
	}

	return result, nil
}

// mountReadOnly mounts the partition read-only at the given mount point.
func mountReadOnly(ctx context.Context, partition, mountPoint string) error {
	cmd := exec.CommandContext(ctx, "mount", "-o", "ro", partition, mountPoint)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("mount: %s", errMsg)
		}
		return fmt.Errorf("mount: %w", err)
	}

	return nil
}

// unmount unmounts the given mount point.
func unmount(mountPoint string) error {
	cmd := exec.Command("umount", mountPoint)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("umount: %s", errMsg)
		}
		return fmt.Errorf("umount: %w", err)
	}

	return nil
}

// checkMetadata runs dumpe2fs -h on the partition to validate filesystem
// metadata integrity. It checks for a valid superblock and clean state.
func checkMetadata(ctx context.Context, partition string) error {
	cmd := exec.CommandContext(ctx, "dumpe2fs", "-h", partition)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("dumpe2fs: %s", errMsg)
		}
		return fmt.Errorf("dumpe2fs: %w", err)
	}

	output := stdout.String()

	// Verify the filesystem state is clean.
	if strings.Contains(output, "Filesystem state:") && !strings.Contains(output, "clean") {
		return fmt.Errorf("filesystem is not in clean state")
	}

	return nil
}

// inferDeviceFromPartition strips the partition suffix to get the parent
// block device. e.g. /dev/sda1 → /dev/sda, /dev/nvme0n1p1 → /dev/nvme0n1.
func inferDeviceFromPartition(partition string) string {
	p := partition

	// NVMe: /dev/nvme0n1p1 → /dev/nvme0n1
	if idx := strings.LastIndex(p, "p"); idx > 0 {
		candidate := p[:idx]
		if strings.Contains(candidate, "nvme") || strings.Contains(candidate, "loop") {
			return candidate
		}
	}

	// SATA/SAS: /dev/sda1 → /dev/sda — strip trailing digits.
	end := len(p)
	for end > 0 && p[end-1] >= '0' && p[end-1] <= '9' {
		end--
	}
	if end < len(p) {
		return p[:end]
	}

	return p
}
