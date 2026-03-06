package engine

import "time"

// StatusReport holds parsed output from `snapraid status`.
type StatusReport struct {
	Files            int              `json:"files"`
	Fragmented       int              `json:"fragmented"`
	ExcessFragments  int              `json:"excess_fragments"`
	FileSize         uint64           `json:"file_size"`
	ParitySize       uint64           `json:"parity_size"`
	WastedSpace      uint64           `json:"wasted_space"`
	UnsyncedBlocks   int              `json:"unsynced_blocks"`
	UnscrubbedPercent float64         `json:"unscrubbed_percent"`
	BadBlocks        int              `json:"bad_blocks"`
	ScrubAge         ScrubAgeReport   `json:"scrub_age"`
	DiskStatus       []DiskInfo       `json:"disk_status"`
}

type ScrubAgeReport struct {
	Oldest time.Time `json:"oldest"`
	Median time.Time `json:"median"`
	Newest time.Time `json:"newest"`
}

type DiskInfo struct {
	Name   string `json:"name"`
	Device string `json:"device"`
	Used   uint64 `json:"used"`
	Free   uint64 `json:"free"`
	Wasted uint64 `json:"wasted"`
}

// DiffReport holds parsed output from `snapraid diff`.
type DiffReport struct {
	Added       int         `json:"added"`
	Removed     int         `json:"removed"`
	Updated     int         `json:"updated"`
	Moved       int         `json:"moved"`
	Copied      int         `json:"copied"`
	Restored    int         `json:"restored"`
	HasChanges  bool        `json:"has_changes"`
	FileDetails []DiffEntry `json:"file_details,omitempty"`
}

type DiffEntry struct {
	Change string `json:"change"`
	Path   string `json:"path"`
}

// SmartReport holds parsed output from `snapraid smart -v`.
type SmartReport struct {
	Disks                  []SmartDisk `json:"disks"`
	OverallFailProbability float64     `json:"overall_fail_probability"`
}

type SmartDisk struct {
	Device             string  `json:"device"`
	DiskName           string  `json:"disk_name"`
	Serial             string  `json:"serial"`
	Temperature        int     `json:"temperature"`
	PowerOnDays        int     `json:"power_on_days"`
	ErrorCount         int     `json:"error_count"`
	FailureProbability float64 `json:"failure_probability"`
	Size               uint64  `json:"size"`
	Status             string  `json:"status"`
}

// SyncReport holds the result of `snapraid sync`.
type SyncReport struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// ScrubReport holds the result of `snapraid scrub`.
type ScrubReport struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// FixReport holds the result of `snapraid fix`.
type FixReport struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// TouchReport holds the result of `snapraid touch`.
type TouchReport struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// SyncOptions configures a sync operation.
type SyncOptions struct {
	PreHash    bool
	ForceZero  bool
	ForceEmpty bool
	ForceFull  bool
}

// ScrubOptions configures a scrub operation.
type ScrubOptions struct {
	Plan          string // "bad", "new", "full", or 0-100
	OlderThanDays int
}

// FixOptions configures a fix operation.
type FixOptions struct {
	BadBlocksOnly bool   // -e flag
	Disk          string // -d <disk>
	Filter        string // -f <filter>
}
