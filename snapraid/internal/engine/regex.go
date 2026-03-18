package engine

import "regexp"

// Compiled regexes for parsing snapraid command output.
// Centralized here so every parser draws from a single source of truth.
var (
	// Progress: matches "nn%" in streaming output (sync, scrub, fix).
	reProgress = regexp.MustCompile(`(\d+)%`)

	// --- status output ---

	reFiles           = regexp.MustCompile(`^\s*(\d+)\s+files`)
	reFragmented      = regexp.MustCompile(`^\s*(\d+)\s+fragmented`)
	reExcessFragments = regexp.MustCompile(`^\s*(\d+)\s+excess\s+fragments`)
	reFileSize        = regexp.MustCompile(`^\s*(\d+)\s+GB\s+in\s+\d+\s+files`)
	reParitySize      = regexp.MustCompile(`^\s*(\d+)\s+GB\s+of\s+parity`)
	reWastedSpace     = regexp.MustCompile(`^\s*(\d+)\s+GB\s+of\s+wasted\s+space`)
	reUnsyncedBlocks  = regexp.MustCompile(`^\s*(\d+)\s+unsynced\s+blocks`)
	reUnscrubbedPct   = regexp.MustCompile(`unscrubbed\s+at\s+([\d.]+)%`)
	reBadBlocks       = regexp.MustCompile(`^\s*(\d+)\s+bad\s+blocks`)
	reScrubOldest     = regexp.MustCompile(`the\s+oldest\s+block\s+was\s+scrubbed\s+(\d+)\s+days\s+ago`)
	reScrubMedian     = regexp.MustCompile(`the\s+median\s+block\s+was\s+scrubbed\s+(\d+)\s+days\s+ago`)
	reScrubNewest     = regexp.MustCompile(`the\s+newest\s+block\s+was\s+scrubbed\s+(\d+)\s+days\s+ago`)

	// Disk table: type, name, device, size, used, free, wasted (GB).
	reDiskLine = regexp.MustCompile(`^\s*(data|parity|2-parity|3-parity|4-parity|5-parity|6-parity)\s+(\S+)\s+(\S+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)`)

	// --- diff output ---

	reDiffCounter = regexp.MustCompile(`^\s*(\d+)\s+(added|removed|updated|moved|copied|restored)$`)
	reDiffEntry   = regexp.MustCompile(`^(add|remove|update|move|copy|restore)\s+(.+)$`)

	// --- smart output ---

	reSmartDisk = regexp.MustCompile(
		`^\s*(\d+|-)C?\s+(\d+|-)\s+(\d+|-)\s+(\d+|-)%?\s+([\d.]+)\s*([TGMK]?B)\s+(\S+)\s+(\S+)\s+(\S+)\s+(OK|SSD|FAIL|PREFAIL|n/a)\s*$`,
	)
	reOverallFail = regexp.MustCompile(`probability.*?(\d+)%`)
)
