package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PoolIOStat is a single pool's I/O counters as reported by `zpool iostat`.
// All values are cumulative since the pool was imported; the hub computes
// rates by diffing consecutive samples, so the agent stays stateless.
type PoolIOStat struct {
	Name       string       `json:"name"`
	Alloc      uint64       `json:"alloc"`
	Free       uint64       `json:"free"`
	ReadOps    uint64       `json:"read_ops"`
	WriteOps   uint64       `json:"write_ops"`
	ReadBytes  uint64       `json:"read_bytes"`
	WriteBytes uint64       `json:"write_bytes"`
	Vdevs      []VdevIOStat `json:"vdevs,omitempty"`
	Timestamp  string       `json:"timestamp"`
}

// VdevIOStat is cumulative I/O for a single vdev under a pool.
type VdevIOStat struct {
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Alloc      uint64       `json:"alloc"`
	Free       uint64       `json:"free"`
	ReadOps    uint64       `json:"read_ops"`
	WriteOps   uint64       `json:"write_ops"`
	ReadBytes  uint64       `json:"read_bytes"`
	WriteBytes uint64       `json:"write_bytes"`
	Disks      []DiskIOStat `json:"disks,omitempty"`
}

// DiskIOStat is cumulative I/O for a single leaf device inside a vdev.
// Alloc/Free are reported as dashes by `zpool iostat` for leaves, so we
// omit them to avoid misleading zeros.
type DiskIOStat struct {
	Name       string `json:"name"`
	ReadOps    uint64 `json:"read_ops"`
	WriteOps   uint64 `json:"write_ops"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// SampleIOStat runs `zpool iostat -Hpv` once and parses the cumulative-since-
// boot counters for every pool. Rate calculation is a hub-side responsibility
// (diff two samples over their timestamp delta) so the agent doesn't need to
// hold state between calls.
func (e *Engine) SampleIOStat(ctx context.Context) ([]PoolIOStat, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// -H: scripted (tab-separated, no column headers)
	// -p: parsable (raw bytes / ops, not human-formatted)
	// -v: include per-vdev and per-disk rows
	out, err := e.runZpool(ctx, "iostat", "-Hpv")
	if err != nil {
		return nil, fmt.Errorf("zpool iostat: %w", err)
	}
	pools, err := parseIOStat(out)
	if err != nil {
		return nil, err
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	for i := range pools {
		pools[i].Timestamp = ts
	}
	return pools, nil
}

// parseIOStat parses `zpool iostat -Hpv` output.
//
// With -H the output is tab-separated and headerless, but the hierarchy is
// conveyed only by row ordering and device naming — there are no indentation
// markers to rely on. We walk the rows sequentially and classify each one:
//
//   - a known vdev-type prefix (mirror-N, raidzN-M, cache, log, spare, special,
//     dedup) starts a new vdev under the current pool;
//   - a row that looks like a path/device and isn't a vdev prefix is a leaf
//     disk under the current vdev (or a single-disk vdev if none is open);
//   - any other row is a new pool.
//
// This mirrors how parseVdevTopology classifies `zpool status` output.
func parseIOStat(output string) ([]PoolIOStat, error) {
	var pools []PoolIOStat
	var currentPool *PoolIOStat
	var currentVdev *VdevIOStat

	flushVdev := func() {
		if currentVdev != nil && currentPool != nil {
			currentPool.Vdevs = append(currentPool.Vdevs, *currentVdev)
			currentVdev = nil
		}
	}
	flushPool := func() {
		flushVdev()
		if currentPool != nil {
			pools = append(pools, *currentPool)
			currentPool = nil
		}
	}

	for _, line := range strings.Split(output, "\n") {
		// zpool iostat without an interval emits a trailing blank line; keep
		// parsing through it so a quirky build that blank-separates pools
		// doesn't cause us to drop data on the floor.
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(strings.TrimLeft(line, " \t"), "\t")
		if len(fields) < 7 {
			// Fall back to any-whitespace split for builds that don't honor -H
			// strictly (older zfs-on-Linux is known to vary here).
			fields = strings.Fields(strings.TrimLeft(line, " \t"))
			if len(fields) < 7 {
				continue
			}
		}
		name := fields[0]
		alloc := parseUint64(fields[1])
		free := parseUint64(fields[2])
		rOps := parseUint64(fields[3])
		wOps := parseUint64(fields[4])
		rB := parseUint64(fields[5])
		wB := parseUint64(fields[6])

		switch {
		case isVdevType(name):
			if currentPool == nil {
				// Vdev row with no preceding pool — malformed, skip.
				continue
			}
			flushVdev()
			currentVdev = &VdevIOStat{
				Name:       name,
				Type:       classifyVdevType(name),
				Alloc:      alloc,
				Free:       free,
				ReadOps:    rOps,
				WriteOps:   wOps,
				ReadBytes:  rB,
				WriteBytes: wB,
			}

		case looksLikeDisk(name):
			if currentPool == nil {
				continue
			}
			if currentVdev == nil {
				// Single-disk stripe vdev — the disk IS the vdev.
				currentVdev = &VdevIOStat{
					Name:       name,
					Type:       "disk",
					Alloc:      alloc,
					Free:       free,
					ReadOps:    rOps,
					WriteOps:   wOps,
					ReadBytes:  rB,
					WriteBytes: wB,
				}
				continue
			}
			currentVdev.Disks = append(currentVdev.Disks, DiskIOStat{
				Name:       name,
				ReadOps:    rOps,
				WriteOps:   wOps,
				ReadBytes:  rB,
				WriteBytes: wB,
			})

		default:
			// Not a vdev type and not disk-shaped → treat as a new pool row.
			flushPool()
			currentPool = &PoolIOStat{
				Name:       name,
				Alloc:      alloc,
				Free:       free,
				ReadOps:    rOps,
				WriteOps:   wOps,
				ReadBytes:  rB,
				WriteBytes: wB,
			}
		}
	}
	flushPool()

	if len(pools) == 0 {
		return nil, errors.New("iostat: no pools parsed")
	}
	return pools, nil
}

// looksLikeDisk returns true for rows whose device name is a path-like leaf
// — absolute paths (/dev/sda), by-id/by-path links, or bare short names that
// aren't recognized vdev prefixes. Kept permissive because ZFS accepts an
// enormous variety of leaf device spellings and this is best-effort.
func looksLikeDisk(name string) bool {
	if strings.HasPrefix(name, "/") {
		return true
	}
	// Bare short names (sda, nvme0n1, etc.) — anything that isn't a vdev type
	// keyword and hit the "default" case would be classified as a pool, so we
	// only accept short names that at least look disk-shaped.
	if strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "nvme") ||
		strings.HasPrefix(name, "vd") || strings.HasPrefix(name, "hd") ||
		strings.HasPrefix(name, "xvd") || strings.HasPrefix(name, "wwn-") ||
		strings.HasPrefix(name, "scsi-") || strings.HasPrefix(name, "ata-") {
		return true
	}
	return false
}

// classifyVdevType reduces a row name to its internal type constant. Used so
// the UI can render "raidz2" / "mirror" / "cache" etc. without re-parsing.
func classifyVdevType(name string) string {
	switch {
	case strings.HasPrefix(name, "mirror"):
		return "mirror"
	case strings.HasPrefix(name, "raidz"):
		if idx := strings.IndexByte(name, '-'); idx > 0 {
			return name[:idx]
		}
		return "raidz"
	case strings.HasPrefix(name, "spare"):
		return "spare"
	case strings.HasPrefix(name, "cache"):
		return "cache"
	case strings.HasPrefix(name, "log"):
		return "log"
	case strings.HasPrefix(name, "special"):
		return "special"
	case strings.HasPrefix(name, "dedup"):
		return "dedup"
	default:
		return "disk"
	}
}
