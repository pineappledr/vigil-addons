package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

// propertyPreviewRequest is the body shape for POST /api/properties/preview-diff.
// Callers send the target name, scope ("dataset" or "pool"), and the desired
// changes; the hub fetches the agent's current values and diffs them.
type propertyPreviewRequest struct {
	Scope      string            `json:"scope"`
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
}

// propertyValueRow is the wire shape of one row returned by the agent's
// GET /api/dataset/properties or /api/pool/properties endpoints. Duplicated
// here so the hub doesn't pull in the agent package.
type propertyValueRow struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"`
}

// propertyDiffEntry is one row of the preview-diff response. `Changed` is
// true iff current != target after normalization; the UI grays out unchanged
// rows. Tier is the catalog tier of the key so the UI can highlight red
// entries without a second catalog lookup.
type propertyDiffEntry struct {
	Key     string `json:"key"`
	Current string `json:"current"`
	Target  string `json:"target"`
	Source  string `json:"source,omitempty"`
	Changed bool   `json:"changed"`
	Tier    string `json:"tier,omitempty"`
}

// propertyPreviewResponse is what /api/properties/preview-diff returns.
// HighestTier lets the UI pick the right confirmation dialog (green → none,
// yellow → simple confirm, red → type-to-confirm).
type propertyPreviewResponse struct {
	Scope       string              `json:"scope"`
	Name        string              `json:"name"`
	Entries     []propertyDiffEntry `json:"entries"`
	HighestTier string              `json:"highest_tier"`
	Command     string              `json:"command"`
}

// handlePropertyPreviewDiff powers the rich "show me what changes" step of
// the property editor. It forwards a current-values GET to the resolved
// agent, unions the response with the target values, classifies each row by
// catalog tier, and returns the diff plus the eventual CLI command.
func (s *Server) handlePropertyPreviewDiff(w http.ResponseWriter, r *http.Request) {
	var req propertyPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope != "dataset" && scope != "pool" {
		addonutil.WriteError(w, http.StatusBadRequest, "scope must be 'dataset' or 'pool'")
		return
	}
	if req.Name == "" || len(req.Properties) == 0 {
		addonutil.WriteError(w, http.StatusBadRequest, "name and properties are required")
		return
	}

	agentID := s.resolveAgentID(r)
	if agentID == "" {
		addonutil.WriteError(w, http.StatusBadGateway, "no agent available")
		return
	}

	// Ask the agent for the current values of the keys the user wants to
	// change. Doing it via the agent (not from cached telemetry) guarantees
	// the "current" column reflects live state — the telemetry cache only
	// carries the handful of props the Collector reads by name.
	current, err := s.fetchAgentProperties(r.Context(), agentID, scope, req.Name, keysOf(req.Properties))
	if err != nil {
		addonutil.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	catalog := hubPropertyCatalog()
	tierByKey := make(map[string]string, len(catalog))
	for _, d := range catalog {
		if string(d.Scope) != scope {
			continue
		}
		tierByKey[d.Key] = string(d.Tier)
	}

	currIndex := make(map[string]propertyValueRow, len(current))
	for _, row := range current {
		currIndex[row.Key] = row
	}

	entries := make([]propertyDiffEntry, 0, len(req.Properties))
	highest := "green"
	for k, target := range req.Properties {
		cur := currIndex[k]
		tier := tierByKey[k]
		if tier == "" {
			// Unknown key — force highest tier so the UI can't treat it as
			// benign. The agent-side validator will reject it anyway.
			tier = "red"
		}
		entry := propertyDiffEntry{
			Key:     k,
			Current: cur.Value,
			Target:  target,
			Source:  cur.Source,
			Changed: normalizePropertyValue(cur.Value) != normalizePropertyValue(target),
			Tier:    tier,
		}
		entries = append(entries, entry)
		if entry.Changed && tierRank(tier) > tierRank(highest) {
			highest = tier
		}
	}

	resp := propertyPreviewResponse{
		Scope:       scope,
		Name:        req.Name,
		Entries:     entries,
		HighestTier: highest,
		Command:     buildPropertyCommand(scope, req.Name, req.Properties),
	}
	addonutil.WriteJSON(w, http.StatusOK, resp)
}

// fetchAgentProperties calls the agent's GET /api/{dataset,pool}/properties
// endpoint for the requested keys. The hub normally proxies that endpoint
// blindly, but here we need to consume the response, so we build and send the
// request ourselves against the agent's advertise address.
func (s *Server) fetchAgentProperties(ctx context.Context, agentID, scope, name string, keys []string) ([]propertyValueRow, error) {
	entry := s.registry.Get(agentID)
	if entry == nil {
		return nil, httpError(http.StatusNotFound, "agent not found: "+agentID)
	}
	if entry.Address == "" {
		return nil, httpError(http.StatusBadGateway, "agent has no advertise address")
	}

	base, err := url.Parse(strings.TrimRight(entry.Address, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, httpError(http.StatusBadGateway, "agent has invalid advertise address")
	}

	q := url.Values{}
	switch scope {
	case "dataset":
		base.Path = path.Join(base.Path, "/api/dataset/properties")
		q.Set("name", name)
	case "pool":
		base.Path = path.Join(base.Path, "/api/pool/properties")
		q.Set("pool", name)
	}
	if len(keys) > 0 {
		q.Set("keys", strings.Join(keys, ","))
	}
	base.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// #nosec G107 -- URL built from a PSK-authenticated registry entry whose
	// scheme is restricted to http/https above, and a query string composed
	// of validated scope names + caller-provided key list.
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, httpError(resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rows []propertyValueRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// buildPropertyCommand renders the CLI a user would run to apply `changes`.
// Used by the preview diff so the confirm dialog can show the exact command.
// The order is deterministic (sorted keys) so two identical previews render
// the same string — easier for users who copy-paste to verify.
func buildPropertyCommand(scope, name string, changes map[string]string) string {
	bin := "zfs"
	if scope == "pool" {
		bin = "zpool"
	}
	keys := keysOf(changes)
	sortStrings(keys)
	var b bytes.Buffer
	for i, k := range keys {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(bin)
		b.WriteString(" set ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(changes[k])
		b.WriteString(" ")
		b.WriteString(name)
	}
	return b.String()
}

// normalizePropertyValue makes the "changed" check resilient to cosmetic
// differences. zfs reports sizes in parsed form ("1073741824") and the UI
// typically sends "1G"; the diff is still Changed==true for genuine changes
// but comparing raw strings would always mark them changed. For v1 we just
// trim whitespace and lowercase — callers can refine later if needed.
func normalizePropertyValue(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortStrings(s []string) {
	// sort.Strings would bring in the "sort" import just for this one call.
	// Bubble is fine for the tiny slices (≤20 keys) we see here.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// hubCatalogEntry is the slim shape the hub needs to classify tiers without
// importing the agent package. Keep in sync with agent/properties.go —
// whenever the agent adds a new editable property, add a matching row here.
type hubCatalogEntry struct {
	Key   string
	Scope string
	Tier  string
}

// hubPropertyCatalog returns the minimum catalog the preview-diff handler
// needs: key, scope, and tier. Duplicated from the agent so the hub can
// classify tiers even when the agent is momentarily unreachable, and so the
// UI doesn't need a second round-trip to resolve the confirm dialog style.
func hubPropertyCatalog() []hubCatalogEntry {
	return []hubCatalogEntry{
		{Key: "compression", Scope: "dataset", Tier: "yellow"},
		{Key: "recordsize", Scope: "dataset", Tier: "yellow"},
		{Key: "atime", Scope: "dataset", Tier: "green"},
		{Key: "relatime", Scope: "dataset", Tier: "green"},
		{Key: "sync", Scope: "dataset", Tier: "yellow"},
		{Key: "xattr", Scope: "dataset", Tier: "yellow"},
		{Key: "acltype", Scope: "dataset", Tier: "yellow"},
		{Key: "logbias", Scope: "dataset", Tier: "yellow"},
		{Key: "primarycache", Scope: "dataset", Tier: "yellow"},
		{Key: "secondarycache", Scope: "dataset", Tier: "yellow"},
		{Key: "dedup", Scope: "dataset", Tier: "red"},
		{Key: "snapdir", Scope: "dataset", Tier: "green"},
		{Key: "quota", Scope: "dataset", Tier: "yellow"},
		{Key: "reservation", Scope: "dataset", Tier: "yellow"},

		{Key: "autotrim", Scope: "pool", Tier: "yellow"},
		{Key: "autoexpand", Scope: "pool", Tier: "yellow"},
		{Key: "autoreplace", Scope: "pool", Tier: "yellow"},
		{Key: "failmode", Scope: "pool", Tier: "red"},
		{Key: "comment", Scope: "pool", Tier: "green"},
		{Key: "delegation", Scope: "pool", Tier: "yellow"},
	}
}

// tierRank maps the string tier to a comparable integer. Mirrors the agent's
// private tierRank but lives here to avoid an agent-package import.
func tierRank(t string) int {
	switch t {
	case "red":
		return 2
	case "yellow":
		return 1
	default:
		return 0
	}
}

// httpError is a local sentinel so fetchAgentProperties can return rich
// errors without building up extra structure. The hub logs the full message
// and the client sees whatever the surrounding handler decides.
type httpStatusError struct {
	Status  int
	Message string
}

func (e *httpStatusError) Error() string { return e.Message }

func httpError(status int, msg string) error {
	return &httpStatusError{Status: status, Message: msg}
}
