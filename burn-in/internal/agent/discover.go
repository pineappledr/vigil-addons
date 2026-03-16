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
	Path          string `json:"path"`
	Model         string `json:"model"`
	Serial        string `json:"serial"`
	CapacityBytes int64  `json:"capacity_bytes"`
}

// DiscoverDrives scans /dev/disk/by-id/ for ATA and NVMe base drives,
// then retrieves SMART metadata for each via smartctl.
func DiscoverDrives(logger *slog.Logger) ([]DriveInfo, error) {
	entries, err := os.ReadDir(diskByIDPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", diskByIDPath, err)
	}

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
		drives = append(drives, *info)

		logger.Info("discovered drive",
			"path", info.Path,
			"model", info.Model,
			"serial", info.Serial,
			"capacity_bytes", info.CapacityBytes,
		)
	}

	return drives, nil
}

// isBaseDrive returns true for ata- and nvme- symlinks that are not
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
