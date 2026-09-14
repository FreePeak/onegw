package server

// Settings sections — the dashboard twin of the file-only config blocks
// ([server], [rotation], [usage], [saver], [update]). Same contract as the
// provider editor: line-based splice into the live TOML (comments and keys
// outside the whitelist untouched), config.Load validates BEFORE the rename,
// a successful save hot-reloads and publishes an SSE config event.
//
//	GET /admin/config/sections — effective values per section, secrets masked
//	PUT /admin/config/sections — {section, settings:{key: scalar}}
//
// listen/data_dir/admin_password are intentionally excluded (rebinding needs
// a restart; the password has its own session-aware card). A value equal to
// the built-in default removes the key, so the file keeps only what the
// operator actually changed.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"onegw/internal/config"
)

type sectionKind int

const (
	kindDur  sectionKind = iota // quoted Go duration
	kindInt                     // bare integer
	kindBool                    // bare true/false
	kindStr                     // quoted string
	kindSec                     // secret string: masked in GET; echo keeps it
)

// sectionSpec describes one editable key. eff reads the effective value from
// the loaded+defaults-applied config so GET matches what the gateway runs
// with, even when the key is absent from the file.
type sectionSpec struct {
	header string
	kind   sectionKind
	def    string
	eff    func(*config.Config) string
}

// effective renders the current value; a zero/absent string reports "".
func (sp sectionSpec) effective(c *config.Config) string {
	return sp.eff(c)
}

func (s *Server) handleAdminSectionsGet(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	st := s.cur()
	if st == nil {
		adminError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	out := map[string]map[string]string{}
	for section, keys := range sectionSpecs {
		m := map[string]string{}
		for key, sp := range keys {
			v := sp.effective(st.cfg)
			// An absent scalar ("" for string/duration, "0" for int) means
			// "not set in the file" — the gateway then uses the built-in
			// default, so GET reports that rather than the empty sentinel.
			// Secrets (def "") and empty-def strings (export_url) stay "".
			if (v == "" && sp.def != "") || (sp.kind == kindInt && v == "0" && sp.def != "0") {
				v = sp.def
			}
			if sp.kind == kindSec {
				if v != "" {
					v = "••••••"
				}
			}
			m[key] = v
		}
		out[section] = m
	}
	writeJSON(w, map[string]any{"sections": out})
}

func (s *Server) handleAdminSectionsPut(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req struct {
		Section  string                     `json:"section"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	keys, ok := sectionSpecs[req.Section]
	if !ok {
		adminError(w, http.StatusBadRequest, "unknown section "+req.Section)
		return
	}
	if len(req.Settings) == 0 {
		adminError(w, http.StatusBadRequest, "settings are empty")
		return
	}
	edits := make([]sectionEdit, 0, len(req.Settings))
	for key, raw := range req.Settings {
		sp, known := keys[key]
		if !known {
			adminError(w, http.StatusBadRequest, req.Section+" has no editable key "+key)
			return
		}
		line, err := sp.render(raw)
		if err != nil {
			adminError(w, http.StatusBadRequest, key+": "+err.Error())
			return
		}
		if line == "" && sp.kind == kindSec {
			continue // secrets are keep-on-blank / masked-echo: never cleared here
		}
		edits = append(edits, sectionEdit{sp.header, key, line})
	}
	var changed []string
	if _, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		var e error
		lines, changed, e = spliceSectionEdits(lines, edits)
		return lines, e
	}); err != nil {
		editFailed(w, err)
		return
	}
	s.events.publish("config", `{"sections":`+jsonString(req.Section)+`}`)
	log.Printf("admin: [%s] updated via dashboard: %v", req.Section, changed)
	writeJSON(w, map[string]any{"section": req.Section, "changed": changed, "reload": true})
}

// render turns one incoming scalar into its TOML `key = value` line, or ""
// (remove the key) when it matches the default / is empty / is the masked
// echo of a secret.
func (sp sectionSpec) render(raw json.RawMessage) (string, error) {
	switch sp.kind {
	case kindBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return "", fmt.Errorf("must be true/false")
		}
		if b == (sp.def == "true") {
			return "", nil
		}
		return strconv.FormatBool(b), nil
	case kindInt:
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return "", fmt.Errorf("must be a number")
		}
		if n.String() == "0" || n.String() == sp.def {
			return "", nil
		}
		return n.String(), nil
	default: // kindDur, kindStr, kindSec
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", fmt.Errorf("must be a string")
		}
		if s == "" || s == "••••••" || (sp.kind != kindSec && s == sp.def) {
			return "", nil
		}
		return strconv.Quote(s), nil
	}
}

// sectionTOMLKey maps a UI key to the bare name inside its block: keys of a
// nested table are prefixed ("external.enabled" in [saver.external]), but the
// TOML line is just `enabled`. Top-level blocks pass the key through.
func sectionTOMLKey(header, key string) string {
	if i := strings.IndexByte(header, '.'); i >= 0 {
		return strings.TrimPrefix(key, header[i+1:]+".")
	}
	return key
}

// sectionEdit is one splice instruction: a header block, a key, and the
// rendered value ("" = remove the key, letting the default apply).
type sectionEdit struct{ header, key, rendered string }

// spliceSectionEdits upserts/removes scalar lines inside each [header] block,
// appending a fresh block when the file has none. validateLines (config.Load)
// is the final gate before the caller writes.
func spliceSectionEdits(lines []string, edits []sectionEdit) ([]string, []string, error) {
	changed := []string{}
	for _, ed := range edits {
		tomlKey := sectionTOMLKey(ed.header, ed.key)
		if blocks := scanBlocks(lines, "["+ed.header+"]"); len(blocks) > 0 {
			b := blocks[0]
			block := cloneLines(lines[b.start:b.end])
			if ed.rendered != "" {
				block = upsertScalar(block, tomlKey, tomlKey+" = "+ed.rendered)
				changed = append(changed, ed.key)
			} else {
				block = removeScalar(block, tomlKey)
				changed = append(changed, ed.key+"=default")
			}
			lines = append(append(cloneLines(lines[:b.start]), block...), lines[b.end:]...)
			continue
		}
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "["+ed.header+"]")
		if ed.rendered != "" {
			lines = append(lines, tomlKey+" = "+ed.rendered)
			changed = append(changed, ed.key)
		}
	}
	if err := validateLines(lines); err != nil {
		return nil, nil, err
	}
	return lines, changed, nil
}

// sectionSpecs is the editable whitelist. defs mirror internal/config
// Defaults(); eff reads the effective value. Keep in sync when a config key
// gains UI ambition.
var sectionSpecs = map[string]map[string]sectionSpec{
	"server": {
		"max_body_bytes":          {"server", kindInt, "33554432", func(c *config.Config) string { return itoi(c.Server.MaxBody) }},
		"buffered_budget_bytes":   {"server", kindInt, "50331648", func(c *config.Config) string { return itoi(c.Server.BufferCap) }},
		"access_log":              {"server", kindBool, "false", btos(func(c *config.Config) bool { return c.Server.AccessLog })},
		"stream_requests":         {"server", kindBool, "false", btos(func(c *config.Config) bool { return c.Server.StreamRequests })},
		"response_header_timeout": {"server", kindDur, "60s", func(c *config.Config) string { return c.Server.ResponseHeaderTimeout }},
		"task_routing":            {"server", kindStr, "off", func(c *config.Config) string { return c.Server.TaskRouting }},
		"idempotency_ttl":         {"server", kindDur, "5s", func(c *config.Config) string { return c.Server.IdempotencyTTL }},
		"idempotency_cache":       {"server", kindInt, "128", func(c *config.Config) string { return itoi(int64(c.Server.IdempotencyCache)) }},
	},
	"rotation": {
		"cooldown_base":   {"rotation", kindDur, "10s", func(c *config.Config) string { return c.Rotation.CooldownBase }},
		"cooldown_cap":    {"rotation", kindDur, "60s", func(c *config.Config) string { return c.Rotation.CooldownCap }},
		"flap_threshold":  {"rotation", kindInt, "4", func(c *config.Config) string { return itoi(int64(c.Rotation.FlapThreshold)) }},
		"flap_open":       {"rotation", kindDur, "15s", func(c *config.Config) string { return c.Rotation.FlapOpen }},
		"model_bench_ttl": {"rotation", kindDur, "5m", func(c *config.Config) string { return c.Rotation.ModelBenchTTL }},
		"billing_parole":  {"rotation", kindDur, "30m", func(c *config.Config) string { return c.Rotation.BillingParole }},
	},
	"usage": {
		"flush_interval":  {"usage", kindDur, "5s", func(c *config.Config) string { return c.Usage.FlushInterval }},
		"retention_days":  {"usage", kindInt, "90", func(c *config.Config) string { return itoi(int64(c.Usage.RetentionDays)) }},
		"export_url":      {"usage", kindStr, "", func(c *config.Config) string { return c.Usage.ExportURL }},
		"export_password": {"usage", kindSec, "", func(c *config.Config) string { return c.Usage.ExportPassword }},
	},
	"saver": {
		"enabled":             {"saver", kindBool, "false", btos(func(c *config.Config) bool { return c.Saver.Enabled })},
		"external.enabled":    {"saver.external", kindBool, "false", btos(func(c *config.Config) bool { return c.Saver.External.Enabled })},
		"external.url":        {"saver.external", kindStr, "", func(c *config.Config) string { return c.Saver.External.URL }},
		"external.timeout_ms": {"saver.external", kindInt, "3000", func(c *config.Config) string { return itoi(int64(c.Saver.External.TimeoutMS)) }},
		"external.min_bytes":  {"saver.external", kindInt, "4096", func(c *config.Config) string { return itoi(int64(c.Saver.External.MinBytes)) }},
		"external.fail_open":  {"saver.external", kindBool, "false", btos(func(c *config.Config) bool { return c.Saver.External.FailOpen != nil && *c.Saver.External.FailOpen })},
	},
	"update": {
		"check_interval": {"update", kindDur, "24h", func(c *config.Config) string { return c.Update.CheckInterval }},
		"auto":           {"update", kindBool, "false", btos(func(c *config.Config) bool { return c.Update.Auto })},
		"repo":           {"update", kindStr, "", func(c *config.Config) string { return c.Update.Repo }},
	},
}

func itoi(n int64) string { return strconv.FormatInt(n, 10) }
func btos(f func(*config.Config) bool) func(*config.Config) string {
	return func(c *config.Config) string { return strconv.FormatBool(f(c)) }
}
