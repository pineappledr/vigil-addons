package drive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// RotationType describes the physical medium of a drive.
type RotationType string

const (
	RotationSSD     RotationType = "SSD"
	RotationHDD     RotationType = "HDD"
	RotationUnknown RotationType = "unknown"
)

// DriveInfo holds block device metadata resolved from smartctl.
type DriveInfo struct {
	Path          string       `json:"path"`           // Resolved /dev/ block device path.
	ByIDPath      string       `json:"by_id_path"`     // Original /dev/disk/by-id/ symlink, if applicable.
	Model         string       `json:"model"`
	Serial        string       `json:"serial"`
	CapacityBytes int64        `json:"capacity_bytes"`
	Rotation      RotationType `json:"rotation"`       // SSD, HDD, or unknown.
}

// IsSSD returns true if the drive is a solid state device.
func (d *DriveInfo) IsSSD() bool {
	return d.Rotation == RotationSSD
}

// SmartctlIdentify is the JSON structure returned by smartctl -i --json.
type SmartctlIdentify struct {
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	UserCapacity struct {
		Bytes int64 `json:"bytes"`
	} `json:"user_capacity"`
	RotationRate int    `json:"rotation_rate"` // 0 = SSD/NVMe, >0 = RPM for HDDs.
	// Fallback fields for NVMe drives.
	ModelFamily string `json:"model_family"`
}

// ResolveDrive takes a device path (which may be a /dev/disk/by-id/ symlink)
// and returns a fully populated DriveInfo after resolving the symlink and
// querying smartctl for metadata.
func ResolveDrive(path string) (*DriveInfo, error) {
	cleanPath := filepath.Clean(path)

	if !isValidDevicePath(cleanPath) {
		return nil, fmt.Errorf("invalid device path: %q", path)
	}

	resolved, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("resolving symlink %s: %w", cleanPath, err)
	}

	info := &DriveInfo{
		Path: resolved,
	}

	// Preserve the by-id path if the input was a symlink.
	if resolved != cleanPath {
		info.ByIDPath = cleanPath
	}

	if err := querySmartctlJSON(resolved, info); err != nil {
		return nil, fmt.Errorf("querying SMART info for %s: %w", resolved, err)
	}

	return info, nil
}

// IsSafeTarget verifies the drive is not currently mounted or part of an
// active ZFS pool. Returns nil if safe, or an error describing why it is not.
func IsSafeTarget(devicePath string) error {
	cleanPath := filepath.Clean(devicePath)

	if err := checkNotMounted(cleanPath); err != nil {
		return err
	}
	if err := checkNotZFS(cleanPath); err != nil {
		return err
	}
	return nil
}

// querySmartctlJSON runs smartctl -i --json and populates DriveInfo from
// the structured JSON output.
func querySmartctlJSON(devicePath string, info *DriveInfo) error {
	out, err := runSmartctl("-i", "--json", devicePath)
	if err != nil && len(out) == 0 {
		return err
	}

	var ident SmartctlIdentify
	if err := json.Unmarshal(out, &ident); err != nil {
		// Fall back to line-based parsing if JSON is unavailable.
		return querySmartctlText(devicePath, info)
	}

	info.Model = ident.ModelName
	if info.Model == "" {
		info.Model = ident.ModelFamily
	}
	info.Serial = ident.SerialNumber
	info.CapacityBytes = ident.UserCapacity.Bytes

	// Determine rotation type: smartctl returns rotation_rate=0 for SSDs/NVMe,
	// and the RPM value (e.g. 7200) for mechanical drives. Following the
	// Spearfoot convention, treat all drives as HDD unless explicitly SSD.
	if ident.RotationRate == 0 {
		info.Rotation = RotationSSD
	} else {
		info.Rotation = RotationHDD
	}

	if info.Model == "" && info.Serial == "" {
		return querySmartctlText(devicePath, info)
	}

	return nil
}

// querySmartctlText is the fallback parser for older smartctl without JSON.
func querySmartctlText(devicePath string, info *DriveInfo) error {
	out, err := runSmartctl("-i", devicePath)
	if err != nil && len(out) == 0 {
		return err
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
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
		case "Rotation Rate":
			lower := strings.ToLower(value)
			if strings.Contains(lower, "solid state") {
				info.Rotation = RotationSSD
			} else {
				info.Rotation = RotationHDD
			}
		}
	}

	if info.Model == "" && info.Serial == "" {
		return fmt.Errorf("smartctl output contained no recognizable drive metadata")
	}

	return nil
}

// checkNotMounted reads /proc/mounts to verify no partition of the device
// is currently mounted.
func checkNotMounted(devicePath string) error {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// Non-Linux or /proc unavailable; skip check.
		return nil
	}

	baseDev := filepath.Base(devicePath) // e.g. "sda"

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		mountDev := fields[0]
		mountPoint := fields[1]

		// Match exact device or any partition (sda1, sda2, ...).
		if mountDev == devicePath || strings.HasPrefix(filepath.Base(mountDev), baseDev) {
			return fmt.Errorf("device %s is mounted at %s via %s", devicePath, mountPoint, mountDev)
		}
	}

	return nil
}

// checkNotZFS runs zpool status and checks whether the device appears in
// any active pool.
func checkNotZFS(devicePath string) error {
	out, err := exec.Command("zpool", "status").Output()
	if err != nil {
		// zpool not installed or no pools — safe to proceed.
		return nil
	}

	baseDev := filepath.Base(devicePath)
	resolved, _ := filepath.EvalSymlinks(devicePath)
	resolvedBase := filepath.Base(resolved)

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		vdev := fields[0]
		// ZFS may reference drives by /dev/disk/by-id name, /dev/sdX, or partition.
		if vdev == baseDev || vdev == resolvedBase ||
			strings.HasPrefix(vdev, baseDev) ||
			strings.HasPrefix(vdev, resolvedBase) ||
			strings.Contains(vdev, baseDev) {
			return fmt.Errorf("device %s is part of an active ZFS pool (matched vdev %q)", devicePath, vdev)
		}
	}

	return nil
}

// runSmartctl executes smartctl with the given arguments securely.
// Arguments are passed as discrete parameters to prevent injection.
func runSmartctl(args ...string) ([]byte, error) {
	cmd := exec.Command("smartctl", args...)
	out, err := cmd.Output()
	if err != nil {
		// smartctl exits non-zero for many non-fatal reasons (e.g., bit 2 = open fail,
		// bit 6 = self-test error). Return output if available.
		if len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("smartctl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// isValidDevicePath ensures the path is a well-formed device path to
// prevent directory traversal or injection via crafted paths.
func isValidDevicePath(path string) bool {
	return strings.HasPrefix(path, "/dev/")
}

// splitSmartLine splits a "Key:    Value" line from smartctl text output.
func splitSmartLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

// parseCapacityBytes extracts the byte count from smartctl capacity strings.
// Format: "4,000,787,030,016 bytes [4.00 TB]"
func parseCapacityBytes(s string) int64 {
	s = strings.Split(s, "bytes")[0]
	s = strings.Split(s, "[")[0]
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
