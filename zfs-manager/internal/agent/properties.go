package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// PropertyTier ranks a property by the blast radius of changing it:
//
//   - green  — cosmetic or scoped to the user (atime, mountpoint, snapdir).
//     Always safe to flip.
//   - yellow — affects new writes only, existing blocks keep their old
//     layout/compression (compression, recordsize, sync, logbias, cache).
//     Reversible on paper, but new data will diverge until the next rewrite.
//   - red    — can discard data or requires pool export/re-import (dedup=on,
//     ashift change, readonly flips that break in-flight writers). Never
//     green-lit without an explicit confirm in the UI.
type PropertyTier string

const (
	TierGreen  PropertyTier = "green"
	TierYellow PropertyTier = "yellow"
	TierRed    PropertyTier = "red"
)

// PropertyScope tags whether a property lives on a dataset or a pool. A few
// ZFS properties overlap (feature flags, some inherits) but we only catalog
// the editor-friendly ones and they're disjoint.
type PropertyScope string

const (
	ScopeDataset PropertyScope = "dataset"
	ScopePool    PropertyScope = "pool"
)

// PropertyOption is one entry in a select-type property's choice list. Label
// is shown in the UI; Value is what gets passed to `zfs set` / `zpool set`.
type PropertyOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// PropertyDef is a catalog entry for a single editable ZFS property. The
// catalog is the single source of truth for both UI rendering and agent-side
// validation — the UI cannot offer a property the agent won't accept, and
// vice versa.
type PropertyDef struct {
	Key         string           `json:"key"`
	Label       string           `json:"label"`
	Description string           `json:"description,omitempty"`
	Scope       PropertyScope    `json:"scope"`
	Tier        PropertyTier     `json:"tier"`
	Type        string           `json:"type"` // "select" | "text" | "size"
	Options     []PropertyOption `json:"options,omitempty"`
	Pattern     string           `json:"pattern,omitempty"`
	Placeholder string           `json:"placeholder,omitempty"`
}

// PropertyCatalog is the set of all editable properties, split by scope for
// the UI's convenience. The hub forwards this as-is to the frontend.
type PropertyCatalog struct {
	Dataset []PropertyDef `json:"dataset"`
	Pool    []PropertyDef `json:"pool"`
}

// BuildPropertyCatalog returns the canonical editable-property catalog.
//
// Design rules:
//   - Only include properties a homelab operator reasonably wants to flip.
//     `zfs` exposes hundreds of read-only/feature properties that are noise.
//   - Every property must round-trip: if it's in the catalog, both the agent
//     validator and the UI form know how to render and accept it.
//   - Tier is conservative: when in doubt, raise the tier. A yellow-tier
//     property that could have been green wastes a confirmation click; a
//     green-tier property that should have been red risks data.
func BuildPropertyCatalog() PropertyCatalog {
	onOff := []PropertyOption{
		{Value: "on", Label: "On"},
		{Value: "off", Label: "Off"},
	}
	return PropertyCatalog{
		Dataset: []PropertyDef{
			{
				Key: "compression", Label: "Compression", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "off", Label: "Off"},
					{Value: "lz4", Label: "LZ4 (recommended)"},
					{Value: "zstd", Label: "ZSTD"},
					{Value: "zstd-fast", Label: "ZSTD-Fast"},
					{Value: "gzip", Label: "GZIP"},
					{Value: "gzip-9", Label: "GZIP-9 (slowest, smallest)"},
				},
				Description: "Applies only to new blocks. Existing data is not recompressed until rewritten.",
			},
			{
				Key: "recordsize", Label: "Record Size", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "4K", Label: "4K"}, {Value: "8K", Label: "8K"},
					{Value: "16K", Label: "16K (Database)"}, {Value: "32K", Label: "32K"},
					{Value: "64K", Label: "64K (VM/App)"}, {Value: "128K", Label: "128K (General)"},
					{Value: "256K", Label: "256K"}, {Value: "512K", Label: "512K"},
					{Value: "1M", Label: "1M (Media)"},
				},
				Description: "Changes affect only new data. Existing blocks keep their original recordsize.",
			},
			{
				Key: "atime", Label: "Access Time", Scope: ScopeDataset, Tier: TierGreen,
				Type: "select", Options: onOff,
				Description: "Off is recommended — avoids a metadata write on every read.",
			},
			{
				Key: "relatime", Label: "Relative atime", Scope: ScopeDataset, Tier: TierGreen,
				Type: "select", Options: onOff,
				Description: "Only updates atime if older than mtime/ctime — cheaper than full atime.",
			},
			{
				Key: "sync", Label: "Sync", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "standard", Label: "Standard (O_SYNC honored)"},
					{Value: "always", Label: "Always (safest, slowest)"},
					{Value: "disabled", Label: "Disabled (fastest, risks N seconds of writes on crash)"},
				},
				Description: "Disabled loses up to 5s of recent writes on unclean shutdown — never safe for databases or VMs.",
			},
			{
				Key: "xattr", Label: "Extended Attributes", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "on", Label: "On (legacy)"},
					{Value: "sa", Label: "SA (system attribute — faster)"},
					{Value: "off", Label: "Off"},
				},
				Description: "SA stores xattrs inline for ~10× performance on SELinux/ACL-heavy workloads.",
			},
			{
				Key: "acltype", Label: "ACL Type", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "off", Label: "Off"},
					{Value: "posix", Label: "POSIX ACLs"},
					{Value: "nfsv4", Label: "NFSv4 ACLs"},
				},
			},
			{
				Key: "logbias", Label: "Log Bias", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "latency", Label: "Latency (default)"},
					{Value: "throughput", Label: "Throughput (bypasses SLOG for sync writes)"},
				},
				Description: "Throughput skips the SLOG for sync writes — only meaningful if you have one.",
			},
			{
				Key: "primarycache", Label: "Primary Cache (ARC)", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "all", Label: "All (data + metadata)"},
					{Value: "metadata", Label: "Metadata only"},
					{Value: "none", Label: "None"},
				},
				Description: "Limit what this dataset can occupy in ARC. 'none' is almost never what you want.",
			},
			{
				Key: "secondarycache", Label: "Secondary Cache (L2ARC)", Scope: ScopeDataset, Tier: TierYellow,
				Type: "select",
				Options: []PropertyOption{
					{Value: "all", Label: "All"},
					{Value: "metadata", Label: "Metadata only"},
					{Value: "none", Label: "None"},
				},
			},
			{
				Key: "dedup", Label: "Deduplication", Scope: ScopeDataset, Tier: TierRed,
				Type: "select",
				Options: []PropertyOption{
					{Value: "off", Label: "Off (recommended)"},
					{Value: "on", Label: "On (SHA-256)"},
					{Value: "sha512", Label: "SHA-512"},
					{Value: "skein", Label: "Skein"},
				},
				Description: "RED TIER — dedup table lives in RAM (~320 bytes per block). Turning it on is effectively permanent: deduped blocks stay deduped even after setting dedup=off.",
			},
			{
				Key: "snapdir", Label: "Snapshot Directory", Scope: ScopeDataset, Tier: TierGreen,
				Type: "select",
				Options: []PropertyOption{
					{Value: "hidden", Label: "Hidden (default)"},
					{Value: "visible", Label: "Visible (show .zfs)"},
				},
			},
			{
				Key: "quota", Label: "Quota", Scope: ScopeDataset, Tier: TierGreen,
				Type: "size", Placeholder: "e.g. 100G, 1T, or none",
				Pattern:     `^(none|\d+(\.\d+)?[KMGTP]?)$`,
				Description: "Upper limit on space this dataset can consume. Use 'none' to remove.",
			},
			{
				Key: "reservation", Label: "Reservation", Scope: ScopeDataset, Tier: TierGreen,
				Type: "size", Placeholder: "e.g. 50G, or none",
				Pattern:     `^(none|\d+(\.\d+)?[KMGTP]?)$`,
				Description: "Guaranteed minimum space. Use 'none' to remove.",
			},
		},
		Pool: []PropertyDef{
			{
				Key: "autotrim", Label: "Auto TRIM", Scope: ScopePool, Tier: TierGreen,
				Type: "select", Options: onOff,
				Description: "Background TRIM on SSDs. Off by default; turn on for all-SSD pools.",
			},
			{
				Key: "autoexpand", Label: "Auto Expand", Scope: ScopePool, Tier: TierGreen,
				Type: "select", Options: onOff,
				Description: "Automatically grow the pool when all devices in a vdev are replaced with larger ones.",
			},
			{
				Key: "autoreplace", Label: "Auto Replace", Scope: ScopePool, Tier: TierYellow,
				Type: "select", Options: onOff,
				Description: "Automatically initiate a replace when a hot spare kicks in. Requires configured spares.",
			},
			{
				Key: "failmode", Label: "Fail Mode", Scope: ScopePool, Tier: TierRed,
				Type: "select",
				Options: []PropertyOption{
					{Value: "wait", Label: "Wait (block I/O until device returns — default)"},
					{Value: "continue", Label: "Continue (return EIO on writes, reads still proceed)"},
					{Value: "panic", Label: "Panic (kernel panic on I/O failure)"},
				},
				Description: "RED TIER — 'panic' will force a host reboot on the first pool I/O error. Only use on hosts where a panicked boot is preferable to corruption (e.g. HA clusters).",
			},
			{
				Key: "comment", Label: "Comment", Scope: ScopePool, Tier: TierGreen,
				Type: "text", Placeholder: "Free-form note shown in zpool list -o comment",
				Description: "Cosmetic annotation — no runtime effect.",
			},
			{
				Key: "delegation", Label: "Delegation", Scope: ScopePool, Tier: TierYellow,
				Type: "select", Options: onOff,
				Description: "Whether zfs allow delegations are honored. Off disables per-user permissions.",
			},
		},
	}
}

// sizeValuePattern is the accepted shape for quota/reservation/size-like
// properties. Matches "none", an optional decimal, and an optional unit
// suffix. Case-insensitive match is applied by the validator.
var sizeValuePattern = regexp.MustCompile(`(?i)^(none|\d+(\.\d+)?[kmgtp]?)$`)

// onOffLike recognises plain boolean-ish values, used as a fallback when a
// PropertyDef forgot to declare select options (defense in depth — the
// catalog is our main line but we don't want typos there to corrupt a pool).
var onOffLike = map[string]bool{"on": true, "off": true}

// ValidateProperty returns nil iff `value` is a legal value for `key` under
// the given scope. Unknown keys are rejected — the agent refuses to pass
// arbitrary names to `zfs set` / `zpool set` because mistyping a property
// name on a pool property is historically how people lose hours debugging
// "why did nothing happen".
func ValidateProperty(catalog PropertyCatalog, scope PropertyScope, key, value string) error {
	defs := catalog.Dataset
	if scope == ScopePool {
		defs = catalog.Pool
	}
	var def *PropertyDef
	for i := range defs {
		if defs[i].Key == key {
			def = &defs[i]
			break
		}
	}
	if def == nil {
		return fmt.Errorf("unknown %s property %q", scope, key)
	}
	if value == "" {
		return fmt.Errorf("%s: empty value", key)
	}

	switch def.Type {
	case "select":
		if len(def.Options) == 0 {
			// Catalog bug — treat as on/off fallback so we don't silently pass
			// an unvalidated value to zfs.
			if !onOffLike[strings.ToLower(value)] {
				return fmt.Errorf("%s: %q not in on/off", key, value)
			}
			return nil
		}
		for _, opt := range def.Options {
			if opt.Value == value {
				return nil
			}
		}
		return fmt.Errorf("%s: %q is not one of the allowed values", key, value)

	case "size":
		if !sizeValuePattern.MatchString(value) {
			return fmt.Errorf("%s: %q is not a valid size (expected 'none' or a number with optional K/M/G/T/P suffix)", key, value)
		}
		return nil

	case "text":
		// Text properties accept anything non-empty — we don't second-guess
		// user-authored comments. Still strip obviously dangerous shell
		// injection attempts since these values end up in argv.
		if strings.ContainsAny(value, "\n\r\x00") {
			return fmt.Errorf("%s: control characters are not allowed", key)
		}
		return nil

	default:
		return fmt.Errorf("%s: catalog type %q is not handled — this is a bug", key, def.Type)
	}
}

// ClassifyChange reports the highest tier among the supplied property changes.
// Callers use this to decide whether to require type-to-confirm (red) or a
// simple dialog confirm (yellow) or to skip confirmation entirely (green).
func ClassifyChange(catalog PropertyCatalog, scope PropertyScope, changes map[string]string) PropertyTier {
	defs := catalog.Dataset
	if scope == ScopePool {
		defs = catalog.Pool
	}
	index := make(map[string]PropertyTier, len(defs))
	for _, d := range defs {
		index[d.Key] = d.Tier
	}

	tier := TierGreen
	for k := range changes {
		t, ok := index[k]
		if !ok {
			// Unknown properties are treated as red so an unknown change
			// can't sneak through a "safe" green-tier path.
			return TierRed
		}
		if tierRank(t) > tierRank(tier) {
			tier = t
		}
	}
	return tier
}

func tierRank(t PropertyTier) int {
	switch t {
	case TierRed:
		return 2
	case TierYellow:
		return 1
	default:
		return 0
	}
}
