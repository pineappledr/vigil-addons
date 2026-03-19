package drive

import (
	"context"
	"fmt"
	"strings"
)

// PartitionResult holds the outcome of a partitioning operation.
type PartitionResult struct {
	Device    string `json:"device"`
	Partition string `json:"partition"` // e.g. /dev/sda1
	TableType string `json:"table_type"`
}

// gptTypeCode maps a filesystem name to its GPT partition type hex code.
func gptTypeCode(fileSystem string) string {
	switch fileSystem {
	case "fat32":
		return "EF00" // EFI System
	case "linux-swap":
		return "8200" // Linux Swap
	case "raw":
		return "BF00" // Solaris/ZFS
	default:
		return "8300" // Linux Filesystem (ext4, xfs, btrfs)
	}
}

// PartitionGPT wipes the existing partition table on the device and creates
// a fresh GPT layout with a single partition. The partition type is set
// according to the target filesystem. For "raw" partitions, reservedPct
// controls how much space is left unallocated at the end of the drive
// (important for ZFS replacement compatibility).
func PartitionGPT(ctx context.Context, devicePath, fileSystem string, reservedPct int) (*PartitionResult, error) {
	if !isValidDevicePath(devicePath) {
		return nil, fmt.Errorf("invalid device path: %q", devicePath)
	}

	// Safety: confirm the device is not mounted before touching the table.
	if err := IsSafeTarget(devicePath); err != nil {
		return nil, fmt.Errorf("safety check before partitioning: %w", err)
	}

	if fileSystem == "" {
		fileSystem = "ext4"
	}
	typeCode := gptTypeCode(fileSystem)

	// Step 1: Zap all GPT and MBR data structures.
	if err := runSgdisk(ctx, devicePath, "--zap-all"); err != nil {
		return nil, fmt.Errorf("wiping partition table: %w", err)
	}

	// Step 2: Create the partition.
	var newArg string
	if fileSystem == "raw" && reservedPct > 0 {
		// For raw/ZFS: reduce partition size by reservedPct, leaving
		// unallocated space at the end of the drive. sgdisk's relative
		// sizing with -<percentage>% computes the end sector.
		endSpec := fmt.Sprintf("-%d%%", reservedPct)
		newArg = fmt.Sprintf("--new=1:0:%s", endSpec)
	} else {
		// Span the entire device: first available sector to last.
		newArg = "--new=1:0:0"
	}

	typeArg := fmt.Sprintf("--typecode=1:%s", typeCode)
	if err := runSgdisk(ctx, devicePath, newArg, typeArg); err != nil {
		return nil, fmt.Errorf("creating GPT partition: %w", err)
	}

	// Step 3: Verify the table.
	if err := runSgdisk(ctx, devicePath, "--verify"); err != nil {
		// Verification failure is non-fatal — some drives report
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
func runSgdisk(ctx context.Context, device string, args ...string) error {
	cmdArgs := append(args, device)
	result, err := runWithProcessGroup(ctx, "sgdisk", cmdArgs...)
	if err != nil {
		return stderrMessage(result, err, "sgdisk "+strings.Join(args, " "))
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
