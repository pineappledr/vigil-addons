package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const diskByIDPath = "/dev/disk/by-id/"

// DriveInfo describes a discovered drive.
type DriveInfo struct {
	Path          string   `json:"path"`
	Model         string   `json:"model"`
	Serial        string   `json:"serial"`
	CapacityBytes int64    `json:"capacity_bytes"`
	Transport     string   `json:"transport"`                // "SATA", "NVMe", "USB", "SAS", or "unknown"
	IsOSDrive     bool     `json:"is_os_drive"`              // true if any partition hosts /, /boot, or swap
	MountPoints   []string `json:"mount_points,omitempty"`   // active mount points (e.g. ["/", "/boot/efi"])
}

// DiscoverDrives scans /dev/disk/by-id/ for ATA, NVMe, and USB base drives,
// then retrieves SMART metadata for each via smartctl.
func DiscoverDrives(logger *slog.Logger) ([]DriveInfo, error) {
	entries, err := os.ReadDir(diskByIDPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", diskByIDPath, err)
	}

	mounts := loadMountTable()

	var drives []DriveInfo
	for _, entry := range entries {
		name := entry.Name()

		if !isBaseDrive(name) {
			continue
		}

		fullPath := filepath.Join(diskByIDPath, name)

		// Resolve symlink to the actual block device.
		resolved, err := filepath.EvalSymlinks(fullPath)
		if err != nil {
			logger.Warn("failed to resolve symlink", "path", fullPath, "error", err)
			continue
		}

		info, err := querySmartInfo(fullPath)
		if err != nil {
			logger.Warn("failed to retrieve SMART info", "path", fullPath, "resolved", resolved, "error", err)
			continue
		}

		// Use the by-id path for stable identification.
		info.Path = fullPath
		info.Transport = transportFromName(name)
		info.MountPoints, info.IsOSDrive = checkMounts(resolved, mounts)

		drives = append(drives, *info)

		logger.Info("discovered drive",
			"path", info.Path,
			"model", info.Model,
			"serial", info.Serial,
			"capacity_bytes", info.CapacityBytes,
			"transport", info.Transport,
			"is_os_drive", info.IsOSDrive,
			"mount_points", info.MountPoints,
		)
	}

	return drives, nil
}

// transportFromName derives the bus/transport type from the /dev/disk/by-id/ name prefix.
func transportFromName(name string) string {
	switch {
	case strings.HasPrefix(name, "ata-"):
		return "SATA"
	case strings.HasPrefix(name, "nvme-"):
		return "NVMe"
	case strings.HasPrefix(name, "usb-"):
		return "USB"
	case strings.HasPrefix(name, "scsi-"):
		return "SAS"
	default:
		return "unknown"
	}
}

// mountEntry holds a parsed line from /proc/mounts.
type mountEntry struct {
	device     string // e.g. /dev/sda1
	mountPoint string // e.g. /
}

// osMountPoints are mount points that indicate the drive hosts the OS.
var osMountPoints = map[string]bool{
	"/":     true,
	"/boot": true,
	"/var":  true,
	"/usr":  true,
	"/home": true,
}

// loadMountTable reads /proc/mounts, resolves symlinks on device paths
// (e.g. /dev/disk/by-uuid/... → /dev/sda1), and returns all mount entries.
func loadMountTable() []mountEntry {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}

	var entries []mountEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}
		// Resolve symlinks so /dev/disk/by-uuid/... becomes /dev/sda1 etc.
		if resolved, err := filepath.EvalSymlinks(dev); err == nil {
			dev = resolved
		}
		entries = append(entries, mountEntry{device: dev, mountPoint: fields[1]})
	}
	return entries
}

// checkMounts returns the mount points for any partition belonging to the
// given block device and whether any of them indicates an OS drive.
func checkMounts(resolvedDev string, mounts []mountEntry) (mountPoints []string, isOS bool) {
	baseDev := filepath.Base(resolvedDev) // e.g. "sda"

	for _, m := range mounts {
		mountBase := filepath.Base(m.device) // already resolved by loadMountTable

		// Match the device itself or any of its partitions (sda, sda1, sda2, ...).
		if mountBase == baseDev || strings.HasPrefix(mountBase, baseDev) {
			mountPoints = append(mountPoints, m.mountPoint)
			if osMountPoints[m.mountPoint] {
				isOS = true
			}
			// Also flag swap
			if m.mountPoint == "swap" || strings.Contains(m.mountPoint, "[SWAP]") {
				isOS = true
			}
		}
	}
	return mountPoints, isOS
}

// isBaseDrive returns true for ata-, nvme-, and usb- symlinks that are not
// partitions (contain -partN) or WWN entries.
func isBaseDrive(name string) bool {
	if strings.HasPrefix(name, "wwn-") {
		return false
	}
	if !strings.HasPrefix(name, "ata-") && !strings.HasPrefix(name, "nvme-") && !strings.HasPrefix(name, "usb-") {
		return false
	}
	if strings.Contains(name, "-part") {
		return false
	}
	// Skip nvme namespace entries like nvme-eui.*
	if strings.HasPrefix(name, "nvme-eui.") {
		return false
	}
	return true
}

// querySmartInfo runs smartctl -i against the given device path and
// parses model, serial, and capacity from the output.
func querySmartInfo(devicePath string) (*DriveInfo, error) {
	cmd := exec.Command("smartctl", "-i", devicePath)
	out, err := cmd.Output()
	if err != nil {
		// smartctl may exit non-zero but still produce usable output.
		if len(out) == 0 {
			return nil, fmt.Errorf("smartctl -i %s: %w", devicePath, err)
		}
	}

	return parseSmartInfo(out)
}

// parseSmartInfo extracts drive metadata from smartctl -i output.
func parseSmartInfo(output []byte) (*DriveInfo, error) {
	info := &DriveInfo{}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()

		key, value, ok := splitSmartLine(line)
		if !ok {
			continue
		}

		switch key {
		case "Device Model", "Model Number", "Product":
			info.Model = value
		case "Serial Number", "Serial number":
			info.Serial = value
		case "User Capacity":
			info.CapacityBytes = parseCapacityBytes(value)
		case "Total NVM Capacity", "Namespace 1 Size/Capacity":
			if info.CapacityBytes == 0 {
				info.CapacityBytes = parseCapacityBytes(value)
			}
		}
	}

	if info.Model == "" && info.Serial == "" {
		return nil, fmt.Errorf("smartctl output contained no recognizable drive metadata")
	}

	return info, nil
}

// splitSmartLine splits a "Key:    Value" line from smartctl output.
func splitSmartLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

// parseCapacityBytes extracts the byte count from smartctl capacity strings.
// Format examples:
//
//	"4,000,787,030,016 bytes [4.00 TB]"
//	"500,107,862,016 bytes [500 GB]"
func parseCapacityBytes(s string) int64 {
	// Take everything before "bytes" or the bracket.
	s = strings.Split(s, "bytes")[0]
	s = strings.Split(s, "[")[0]
	s = strings.TrimSpace(s)

	// Remove commas and spaces.
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
