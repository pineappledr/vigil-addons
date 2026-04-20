package agent

import (
	"context"
	"fmt"
	"strings"
)

// PropertyValue is one row of `zfs get` / `zpool get` output, keyed by the
// property name with the raw string value and the source (local, default,
// inherited from another dataset). Source is surfaced to the UI so users can
// tell when a visible value actually comes from a parent dataset.
type PropertyValue struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"`
}

// GetDatasetProperties fetches current values for the requested keys on a
// dataset. If `keys` is empty it falls back to the full editable catalog so
// the UI can pre-fill every field without a second round trip.
//
// The underlying command is `zfs get -Hp -o property,value,source k1,k2 name`.
// -H and -p make the output parser-friendly (tab-separated, no headers, raw
// numeric values). `all` is deliberately avoided because some builds emit
// hundreds of feature flags that inflate the payload.
func (e *Engine) GetDatasetProperties(ctx context.Context, name string, keys []string) ([]PropertyValue, error) {
	if name == "" {
		return nil, fmt.Errorf("dataset name required")
	}
	if len(keys) == 0 {
		keys = datasetPropertyKeys()
	}
	args := []string{"get", "-Hp", "-o", "property,value,source", strings.Join(keys, ","), name}
	out, err := e.runZFS(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("zfs get: %w", err)
	}
	return parsePropertyRows(out), nil
}

// GetPoolProperties fetches current values for the requested pool properties.
// Mirrors GetDatasetProperties but talks to `zpool get`.
func (e *Engine) GetPoolProperties(ctx context.Context, pool string, keys []string) ([]PropertyValue, error) {
	if pool == "" {
		return nil, fmt.Errorf("pool name required")
	}
	if len(keys) == 0 {
		keys = poolPropertyKeys()
	}
	args := []string{"get", "-Hp", "-o", "property,value,source", strings.Join(keys, ","), pool}
	out, err := e.runZpool(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("zpool get: %w", err)
	}
	return parsePropertyRows(out), nil
}

// SetPoolProperties applies each key/value pair with `zpool set`. Validation
// is delegated to the catalog — callers must run ValidateProperty before
// invoking this, because once we've executed one `zpool set` a later failure
// leaves the pool in a half-applied state that we cannot trivially undo.
func (e *Engine) SetPoolProperties(ctx context.Context, pool string, props map[string]string) (*CommandResult, error) {
	if pool == "" {
		return nil, fmt.Errorf("pool name required")
	}
	if len(props) == 0 {
		return nil, fmt.Errorf("no properties supplied")
	}

	var results []string
	var lastCmd string
	for k, v := range props {
		args := []string{"set", k + "=" + v, pool}
		lastCmd = e.zpoolPath + " " + strings.Join(args, " ")
		out, err := e.runZpool(ctx, args...)
		if err != nil {
			return &CommandResult{Command: lastCmd, ExitCode: exitCode(err), Output: out, Error: err.Error()}, err
		}
		results = append(results, out)
	}
	return &CommandResult{Command: lastCmd, Output: strings.Join(results, "\n")}, nil
}

// parsePropertyRows turns `zfs/zpool get -H` output into typed values. Rows
// with missing source fall back to empty string; rows with fewer than two
// fields are dropped silently (some builds emit trailing empty lines).
func parsePropertyRows(out string) []PropertyValue {
	rows := make([]PropertyValue, 0)
	for _, line := range splitLines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		pv := PropertyValue{Key: fields[0], Value: fields[1]}
		if len(fields) >= 3 {
			pv.Source = fields[2]
		}
		rows = append(rows, pv)
	}
	return rows
}

// datasetPropertyKeys returns the catalog's dataset keys for bulk reads.
// Kept package-private so callers of GetDatasetProperties can rely on the
// zero-keys contract without re-importing the catalog.
func datasetPropertyKeys() []string {
	catalog := BuildPropertyCatalog()
	keys := make([]string, 0, len(catalog.Dataset))
	for _, d := range catalog.Dataset {
		keys = append(keys, d.Key)
	}
	return keys
}

func poolPropertyKeys() []string {
	catalog := BuildPropertyCatalog()
	keys := make([]string, 0, len(catalog.Pool))
	for _, d := range catalog.Pool {
		keys = append(keys, d.Key)
	}
	return keys
}
