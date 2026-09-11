package server

// Config-edit endpoints for the dashboard's provider/combo popup editor:
//
//	PUT /admin/config/providers — add or update one [[providers]] block
//	PUT /admin/config/combos    — add or update one [[combo]] block
//
// Follows the admin_config.go contract: the TOML file stays the single
// source of truth, mutations are line-based splices that preserve every
// line outside the edited block byte-for-byte (comments, other providers,
// nested [[providers.accounts]] tables), the edited content is validated
// with config.Load BEFORE it touches disk (400 on any validation error,
// original file untouched), the write is atomic, and the new config is
// applied through the same Load+Reload path as SIGHUP — a successful save
// immediately reconfigures the live gateway and publishes an SSE `config`
// event so open dashboards refresh.
//
// Secrets never appear in responses: an empty api_key on UPDATE means
// "keep the existing key" (the UI never displays key material, so it
// cannot echo one back). New accounts with empty keys are written
// keyless (env ONEGW_PROVIDER_<NAME>_KEY or nothing) — config.Load's
// validation still refuses credential-less non-searxng providers.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"onegw/internal/config"
)

// ---------------------------------------------------------------------------
// Request shapes
// ---------------------------------------------------------------------------

// acctEdit is one [[providers.accounts]] entry. APIKey "" on update keeps
// the existing key for that account name.
type acctEdit struct {
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Weight  int    `json:"weight"`
	RPM     int    `json:"rpm"`
}

type providerEditReq struct {
	Name             string     `json:"name"`
	Kind             string     `json:"kind"`
	BaseURL          string     `json:"base_url"`
	APIKey           string     `json:"api_key"` // "" on update = keep existing
	Models           []string   `json:"models"`
	MaxConc          int        `json:"max_concurrency"`
	Sticky           string     `json:"sticky"`
	QuotaWindow      string     `json:"quota_window"`
	QuotaLimitTokens int64      `json:"quota_limit_tokens"`
	QuotaLimitReqs   int64      `json:"quota_limit_requests"`
	Accounts         []acctEdit `json:"accounts"`
}

type comboEditReq struct {
	Name    string   `json:"name"`
	Targets []string `json:"targets"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleAdminProviderEdit(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req providerEditReq
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		adminError(w, http.StatusBadRequest, "provider name is required")
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	var action string
	if _, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		var out []string
		out, action, err = spliceProvider(lines, req)
		return out, err
	}); err != nil {
		editFailed(w, err)
		return
	}
	s.events.publish("config", `{"provider":`+jsonString(req.Name)+`}`)
	log.Printf("admin: provider %s %s (config reloaded)", req.Name, action)
	writeJSON(w, map[string]any{"action": action, "name": req.Name, "reload": true})
}

// handleAdminProviderDisabled answers PATCH
// /admin/config/providers/{name}/disabled with {"disabled": bool} — the
// dashboard grid's quick on/off toggle. A full-field PUT would echo the
// whole provider; this endpoint splices ONLY the `disabled` key of the
// named block, so a toggle can never clobber another field's value or a
// key held elsewhere. Persists through the same validate + atomic write
// + Load/Reload path as every other config edit.
func (s *Server) handleAdminProviderDisabled(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	name := r.PathValue("name")
	if strings.TrimSpace(name) == "" {
		adminError(w, http.StatusBadRequest, "provider name is required")
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	if _, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		return spliceProviderDisabled(lines, name, req.Disabled)
	}); err != nil {
		editFailed(w, err)
		return
	}
	s.events.publish("config", `{"provider":`+jsonString(name)+`}`)
	log.Printf("admin: provider %s disabled=%v (config reloaded)", name, req.Disabled)
	writeJSON(w, map[string]any{"name": name, "disabled": req.Disabled, "reload": true})
}

func (s *Server) handleAdminComboEdit(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req comboEditReq
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		adminError(w, http.StatusBadRequest, "combo name is required")
		return
	}
	if len(req.Targets) == 0 {
		adminError(w, http.StatusBadRequest, "combo needs at least one target")
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	var action string
	if _, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		var out []string
		out, action, err = spliceCombo(lines, req)
		return out, err
	}); err != nil {
		editFailed(w, err)
		return
	}
	s.events.publish("config", `{"combo":`+jsonString(req.Name)+`}`)
	log.Printf("admin: combo %s %s (config reloaded)", req.Name, action)
	writeJSON(w, map[string]any{"action": action, "name": req.Name, "reload": true})
}

// jsonString marshals one string safely into an SSE data payload.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// tsv renders one `key = "value"` TOML line (escapeTOMLBasic includes the
// surrounding quotes).
func tsv(key, val string) string { return key + " = " + escapeTOMLBasic(val) }

// validateLines round-trips candidate config content through config.Load
// on a unique temp file BEFORE the atomic write, so request-induced
// validation failures (unknown kind, duplicate name, combo referencing an
// unknown provider, missing credentials) come back as 400s instead of
// 500s. patchConfigFile's writeConfigAtomically re-runs the same check
// before the rename — this is the user-facing copy.
func validateLines(lines []string) error {
	tmp, err := os.CreateTemp("", "onegw-config-validate-*.toml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(strings.Join(lines, "\n")); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	tmp.Close()
	defer os.Remove(name)
	if _, err := config.Load(name); err != nil {
		return badConfigEdit{err}
	}
	return nil
}

// tomlBlock is a half-open line range [start, end) of one top-level block.
type tomlBlock struct{ start, end int }

// ---------------------------------------------------------------------------
// scanBlocks returns every block of the exact section header (e.g.
// "[[providers]]", "[[combo]]"). A block extends through nested
// sub-tables of the same section — "[[providers.accounts]]" and
// "[providers.extra_headers]" alike — and ends at the next different
// header line or EOF. Comment lines never start
// blocks (headers must begin with "[").
func scanBlocks(lines []string, header string) []tomlBlock {
	root := strings.Trim(header, "[]") + "." // "[[providers]]" → "providers."
	var out []tomlBlock
	cur := -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "[") {
			continue
		}
		switch {
		case t == header:
			if cur >= 0 {
				out = append(out, tomlBlock{cur, i})
			}
			cur = i
		case strings.HasPrefix(t, "[["+root) || strings.HasPrefix(t, "["+root):
			// nested sub-table of the same section, either bracket form
			// ("[[providers.accounts]]", "[providers.extra_headers]") —
			// part of the block (cursor's machineId lives in the latter)
		default:
			if cur >= 0 {
				out = append(out, tomlBlock{cur, i})
				cur = -1
			}
		}
	}
	if cur >= 0 {
		out = append(out, tomlBlock{cur, len(lines)})
	}
	return out
}

// blockName extracts the top-level `name = "..."` value of a block (the
// scan skips the block's own header line and stops at the first nested
// table header — name always precedes [[providers.accounts]]).
func blockName(lines []string, b tomlBlock) (string, bool) {
	for i := b.start + 1; i < b.end; i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") {
			return "", false
		}
		eq := strings.Index(t, "=")
		if eq < 0 {
			continue
		}
		if strings.TrimSpace(t[:eq]) != "name" {
			continue
		}
		if v, ok := parseTOMLString(t[eq+1:]); ok {
			return v, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Provider splice
// ---------------------------------------------------------------------------

// spliceProvider adds or updates one [[providers]] block in place.
// Update rewrites only the editor-managed keys (name, kind, base_url,
// api_key, models, max_concurrency, sticky, quota_*) and the
// [[providers.accounts]] sub-tables; every other line of the block —
// extra_headers, always_thinking, session_header, passthrough, comments —
// is preserved byte-for-byte. Empty api_key fields carry over the
// existing key for the same account name.
func spliceProvider(lines []string, req providerEditReq) ([]string, string, error) {
	for _, b := range scanBlocks(lines, "[[providers]]") {
		name, ok := blockName(lines, b)
		if !ok || name != req.Name {
			continue
		}
		old := parseAccounts(lines[b.start:b.end])
		edited := editProviderBlock(cloneLines(lines[b.start:b.end]), req, old)
		candidate := append(cloneLines(lines[:b.start]), append(edited, lines[b.end:]...)...)
		if err := validateLines(candidate); err != nil {
			return nil, "", err
		}
		out := append(cloneLines(lines[:b.start]), edited...)
		return append(out, lines[b.end:]...), "updated", nil
	}
	// add — append a fresh block at EOF
	if err := validateLines(append(cloneLines(lines), renderProviderBlock(req)...)); err != nil {
		return nil, "", err
	}
	out := cloneLines(lines)
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, "")
	return append(out, renderProviderBlock(req)...), "added", nil
}

func cloneLines(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// parseAccounts harvests existing [[providers.accounts]] entries so
// empty-key updates keep the on-disk secrets.
func parseAccounts(block []string) map[string]config.Acct {
	out := map[string]config.Acct{}
	var cur config.Acct
	inAcct := false
	flush := func() {
		if inAcct && cur.Name != "" {
			out[cur.Name] = cur
		}
	}
	for _, ln := range block {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			flush()
			inAcct = t == "[[providers.accounts]]"
			cur = config.Acct{}
			continue
		}
		if !inAcct {
			continue
		}
		eq := strings.Index(t, "=")
		if eq < 0 {
			continue
		}
		val, _ := parseTOMLString(t[eq+1:])
		switch strings.TrimSpace(t[:eq]) {
		case "name":
			cur.Name = val
		case "api_key":
			cur.APIKey = val
		case "base_url":
			cur.BaseURL = val
		}
	}
	flush()
	return out
}

// editProviderBlock applies the managed upserts to one block's lines.
func editProviderBlock(block []string, req providerEditReq, old map[string]config.Acct) []string {
	// Strip the old account tables first: their stranded content is hoisted
	// into the top region, so the upserts below replace the hoisted
	// `models` line in place instead of writing a second copy.
	if req.Accounts != nil {
		block = removeAccountTables(block)
	}
	block = upsertScalar(block, "kind", tsv("kind", req.Kind))
	if req.BaseURL != "" {
		block = upsertScalar(block, "base_url", tsv("base_url", req.BaseURL))
	} else {
		block = removeScalar(block, "base_url")
	}
	if req.APIKey != "" {
		block = upsertScalar(block, "api_key", tsv("api_key", req.APIKey))
	}
	if len(req.Models) > 0 {
		block = upsertScalar(block, "models", "models = "+renderStringArray(req.Models))
	} else {
		block = removeScalar(block, "models")
	}
	if req.MaxConc > 0 {
		block = upsertScalar(block, "max_concurrency", "max_concurrency = "+strconv.Itoa(req.MaxConc))
	} else {
		block = removeScalar(block, "max_concurrency")
	}
	if req.Sticky != "" {
		block = upsertScalar(block, "sticky", tsv("sticky", req.Sticky))
	} else {
		block = removeScalar(block, "sticky")
	}
	if req.QuotaWindow != "" {
		block = upsertScalar(block, "quota_window", tsv("quota_window", req.QuotaWindow))
	} else {
		block = removeScalar(block, "quota_window")
	}
	if req.QuotaLimitTokens > 0 {
		block = upsertScalar(block, "quota_limit_tokens", "quota_limit_tokens = "+strconv.FormatInt(req.QuotaLimitTokens, 10))
	} else {
		block = removeScalar(block, "quota_limit_tokens")
	}
	if req.QuotaLimitReqs > 0 {
		block = upsertScalar(block, "quota_limit_requests", "quota_limit_requests = "+strconv.FormatInt(req.QuotaLimitReqs, 10))
	} else {
		block = removeScalar(block, "quota_limit_requests")
	}
	// accounts: re-render the nested sub-tables when the request carries any
	if req.Accounts != nil {
		for _, a := range req.Accounts {
			key := a.APIKey
			if key == "" {
				if o, ok := old[a.Name]; ok {
					key = o.APIKey
				}
			}
			block = append(block, renderAccountTable(a, key)...)
		}
	}
	return block
}

// upsertScalar replaces the `key = ...` line inside the block's top-level
// region, or inserts the rendered line right after the name line.
func upsertScalar(block []string, key, rendered string) []string {
	tops := topRegionEnd(block)
	for i := 1; i < tops; i++ {
		t := strings.TrimSpace(block[i])
		if !strings.HasPrefix(t, key) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, key))
		if strings.HasPrefix(rest, "=") {
			block[i] = rendered
			return block
		}
	}
	at := 1 // default: right after the [[...]] header
	for i := 1; i < tops; i++ {
		t := strings.TrimSpace(block[i])
		eq := strings.Index(t, "=")
		if eq > 0 && strings.TrimSpace(t[:eq]) == "name" {
			at = i + 1
			break
		}
	}
	block = append(block, "")
	copy(block[at+1:], block[at:])
	block[at] = rendered
	return block
}

// removeScalar deletes the `key = ...` line from the top-level region.
func removeScalar(block []string, key string) []string {
	tops := topRegionEnd(block)
	var out []string
	for i, ln := range block {
		if i < tops {
			t := strings.TrimSpace(ln)
			if strings.HasPrefix(t, key) {
				rest := strings.TrimSpace(strings.TrimPrefix(t, key))
				if strings.HasPrefix(rest, "=") {
					continue
				}
			}
		}
		out = append(out, ln)
	}
	return out
}

// topRegionEnd returns the index of the first nested-table header in the
// block (or len(block)). Any header form counts: a key below
// "[providers.extra_headers]" belongs to that table, not to the provider.
func topRegionEnd(block []string) int {
	for i := 1; i < len(block); i++ {
		if strings.HasPrefix(strings.TrimSpace(block[i]), "[") {
			return i
		}
	}
	return len(block)
}

// removeAccountTables strips every [[providers.accounts]] sub-table from
// the block (they are re-rendered from the request). Only content an Acct
// actually models is dropped: a hand-written config can leave comments or
// provider-level knobs below an account header — where TOML silently
// re-parents them into that account — and those lines are hoisted back
// into the block's top-level region instead of being deleted with it.
func removeAccountTables(block []string) []string {
	var out, hoisted []string
	inAcct := false
	for _, ln := range block {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") { // any header ends the account region
			inAcct = t == "[[providers.accounts]]"
			if !inAcct {
				out = append(out, ln)
			}
			continue
		}
		if !inAcct {
			out = append(out, ln)
			continue
		}
		if t == "" || isAcctField(t) {
			continue // re-rendered from the request
		}
		hoisted = append(hoisted, ln)
	}
	if len(hoisted) == 0 {
		return out
	}
	// Hoisted lines join the top-level region, where the scalar upserts
	// see them (and TOML gives them back to the provider).
	top := topRegionEnd(out)
	res := make([]string, 0, len(out)+len(hoisted))
	res = append(res, out[:top]...)
	res = append(res, hoisted...)
	return append(res, out[top:]...)
}

// isAcctField reports whether a trimmed line sets one of config.Acct's
// fields — the only content removeAccountTables may drop.
func isAcctField(t string) bool {
	eq := strings.Index(t, "=")
	if eq <= 0 {
		return false
	}
	switch strings.TrimSpace(t[:eq]) {
	case "name", "api_key", "base_url", "weight", "rpm":
		return true
	}
	return false
}

func renderAccountTable(a acctEdit, key string) []string {
	out := []string{"[[providers.accounts]]", tsv("name", a.Name)}
	if key != "" {
		out = append(out, tsv("api_key", key))
	}
	if a.BaseURL != "" {
		out = append(out, tsv("base_url", a.BaseURL))
	}
	if a.Weight != 0 {
		out = append(out, "weight = "+strconv.Itoa(a.Weight))
	}
	if a.RPM != 0 {
		out = append(out, "rpm = "+strconv.Itoa(a.RPM))
	}
	return out
}

// renderProviderBlock renders a whole new [[providers]] block (add path).
func renderProviderBlock(req providerEditReq) []string {
	block := []string{
		"[[providers]]",
		tsv("name", req.Name),
		tsv("kind", req.Kind),
	}
	if req.APIKey != "" {
		block = append(block, tsv("api_key", req.APIKey))
	}
	if req.BaseURL != "" {
		block = append(block, tsv("base_url", req.BaseURL))
	}
	if len(req.Models) > 0 {
		block = append(block, "models = "+renderStringArray(req.Models))
	}
	if req.MaxConc > 0 {
		block = append(block, "max_concurrency = "+strconv.Itoa(req.MaxConc))
	}
	if req.Sticky != "" {
		block = append(block, tsv("sticky", req.Sticky))
	}
	if req.QuotaWindow != "" {
		block = append(block, tsv("quota_window", req.QuotaWindow))
	}
	if req.QuotaLimitTokens > 0 {
		block = append(block, "quota_limit_tokens = "+strconv.FormatInt(req.QuotaLimitTokens, 10))
	}
	if req.QuotaLimitReqs > 0 {
		block = append(block, "quota_limit_requests = "+strconv.FormatInt(req.QuotaLimitReqs, 10))
	}
	for _, a := range req.Accounts {
		block = append(block, renderAccountTable(a, a.APIKey)...)
	}
	return block
}

// spliceProviderDisabled flips the `disabled` key of one existing
// [[providers]] block (true = upsert, false = remove so the default
// enabled state is explicit). Every other line of the block — comments,
// keys, nested accounts — is preserved byte-for-byte. The edited config
// is round-trip validated by the caller's patchConfigFile before the
// atomic write.
func spliceProviderDisabled(lines []string, name string, disabled bool) ([]string, error) {
	for _, b := range scanBlocks(lines, "[[providers]]") {
		n, ok := blockName(lines, b)
		if !ok || n != name {
			continue
		}
		edited := cloneLines(lines[b.start:b.end])
		if disabled {
			edited = upsertScalar(edited, "disabled", "disabled = true")
		} else {
			edited = removeScalar(edited, "disabled")
		}
		candidate := append(cloneLines(lines[:b.start]), append(edited, lines[b.end:]...)...)
		if err := validateLines(candidate); err != nil {
			return nil, err
		}
		return append(cloneLines(lines[:b.start]), append(edited, lines[b.end:]...)...), nil
	}
	return nil, fmt.Errorf("provider %s not found in config", name)
}

// ---------------------------------------------------------------------------
// Combo splice
// ---------------------------------------------------------------------------

// spliceCombo adds or updates one [[combo]] block. Update rewrites only
// the targets line; comments and other lines are preserved.
func spliceCombo(lines []string, req comboEditReq) ([]string, string, error) {
	for _, b := range scanBlocks(lines, "[[combo]]") {
		name, ok := blockName(lines, b)
		if !ok || name != req.Name {
			continue
		}
		edited := upsertScalar(cloneLines(lines[b.start:b.end]), "targets", "targets = "+renderStringArray(req.Targets))
		candidate := append(cloneLines(lines[:b.start]), append(edited, lines[b.end:]...)...)
		if err := validateLines(candidate); err != nil {
			return nil, "", err
		}
		out := append(cloneLines(lines[:b.start]), edited...)
		return append(out, lines[b.end:]...), "updated", nil
	}
	rendered := []string{
		"[[combo]]",
		tsv("name", req.Name),
		"targets = " + renderStringArray(req.Targets),
	}
	if err := validateLines(append(cloneLines(lines), rendered...)); err != nil {
		return nil, "", err
	}
	out := cloneLines(lines)
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, "")
	return append(out, rendered...), "added", nil
}
