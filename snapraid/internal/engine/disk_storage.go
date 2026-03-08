package engine

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// DiskStorageInfo holds OS-level filesystem information for a snapraid data disk.
type DiskStorageInfo struct {
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"`
	Filesystem string `json:"filesystem"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	UsedPct    float64 `json:"used_pct"`
}

// diskConfigEntry represents a parsed "disk" line from snapraid.conf.
type diskConfigEntry struct {
	Name      string
	MountPath string
}

// CollectDiskStorage parses the snapraid config to find data disk mount paths,
// then collects OS-level filesystem information for each.
func (e *Engine) CollectDiskStorage() ([]DiskStorageInfo, error) {
	entries, err := parseDiskEntries(e.configPath)
	if err != nil {
		return nil, fmt.Errorf("parsing disk entries: %w", err)
	}

	mountFS, _ := parseMountFilesystems()

	var result []DiskStorageInfo
	for _, entry := range entries {
		info := DiskStorageInfo{
			Name:      entry.Name,
			MountPath: entry.MountPath,
		}

		if fs, ok := mountFS[entry.MountPath]; ok {
			info.Filesystem = fs
		}

		var stat syscall.Statfs_t
		if err := syscall.Statfs(entry.MountPath, &stat); err == nil {
			info.TotalBytes = stat.Blocks * uint64(stat.Bsize)
			info.FreeBytes = stat.Bavail * uint64(stat.Bsize)
			info.UsedBytes = info.TotalBytes - (stat.Bfree * uint64(stat.Bsize))
			if info.TotalBytes > 0 {
				info.UsedPct = float64(info.UsedBytes) / float64(info.TotalBytes) * 100
			}
		}

		result = append(result, info)
	}

	return result, nil
}

// parseDiskEntries reads a snapraid.conf and extracts "disk" lines.
// Each line looks like: "disk disk01 /media/tank/disk01"
func parseDiskEntries(configFile string) ([]diskConfigEntry, error) {
	f, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []diskConfigEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "disk" {
			continue
		}

		entries = append(entries, diskConfigEntry{
			Name:      fields[1],
			MountPath: fields[2],
		})
	}

	return entries, scanner.Err()
}

// parseMountFilesystems reads /proc/mounts and returns a map of mount_path → filesystem_type.
func parseMountFilesystems() (map[string]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Format: device mount_point fs_type options dump pass
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 {
			result[fields[1]] = fields[2]
		}
	}

	return result, scanner.Err()
}
