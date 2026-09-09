// Admin config surface: a narrow write path over the file-based config.
//
// GET   /admin/config          — live config as TOML, secrets masked.
// PUT   /admin/config/reload   — re-run config.Load + Server.Reload (the
//
//	SIGHUP path) without shell access.
//
// PATCH /admin/config/keys     — add/remove gateway auth keys by splicing
//
//	the [auth] table on disk.
//
// PATCH /admin/config/aliases  — set/delete alias entries ([aliases] table).
//
// Non-goals: no DB-backed config, no general TOML editor. The file stays
// the single source of truth: every mutation is a line-based splice of the
// TOML on disk (comments and untouched sections preserved byte-for-byte),
// round-trip checked with config.Load, written atomically (temp file +
// rename), and applied through the same Load+Reload path as SIGHUP.
// Secrets never appear in responses or logs — counts and lengths only.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"onegw/internal/config"
)

// SetOnConfigReload registers a wiring-layer callback fired after every
// successful config swap: main syncs the outer-mux /admin/update handler's
// config copy from it. Without the hook, the first dashboard reload freezes
// the update handler's credential and [update] settings at their
// process-start values while every other admin route sees the reload (#63).
func (s *Server) SetOnConfigReload(fn func(*config.Config)) {
	if fn != nil {
		s.cfgReloadHook.Store(&fn)
	}
}

func (s *Server) fireOnConfigReload(cfg *config.Config) {
	if p := s.cfgReloadHook.Load(); p != nil {
		(*p)(cfg)
	}
}

// SetConfigPath records the TOML file the process was started from. Called
// once by main; without it the mutating endpoints refuse to run.
func (s *Server) SetConfigPath(path string) {
	if path != "" {
		s.cfgPath.Store(&path)
	}
}

func (s *Server) configPath() string {
	if p := s.cfgPath.Load(); p != nil {
		return *p
	}
	return ""
}

func adminUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

func adminError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// badConfigEdit marks request-induced failures (bad fields, splice
// validation) so handlers answer 400 instead of 500.
type badConfigEdit struct{ error }

// ---------------------------------------------------------------------------
// GET /admin/config — masked live view
// ---------------------------------------------------------------------------

func (s *Server) handleAdminConfigGet(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	path := s.configPath()
	var buf bytes.Buffer
	buf.WriteString("# onegw live config — secrets are masked as ***(len=N); this view is not loadable as-is.\n")
	if path != "" {
		fmt.Fprintf(&buf, "# config_file = %s\n", path)
	}
	fmt.Fprintf(&buf, "# generated = %s\n", time.Now().UTC().Format(time.RFC3339))
	if err := toml.NewEncoder(&buf).Encode(maskedConfig(s.cur().cfg)); err != nil {
		adminError(w, http.StatusInternalServerError, "config encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/toml")
	if path != "" {
		w.Header().Set("X-OneGW-Config-Path", path)
	}
	_, _ = w.Write(buf.Bytes())
}

// maskSecret replaces a secret with a length marker. Length leakage is the
// documented tradeoff of the mask format; no key material leaves the
// process.
func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	return fmt.Sprintf("***(len=%d)", len(v))
}

// secretishHeader reports whether an extra_headers value may carry
// credentials (Authorization, X-Api-Key, ...) and must be masked.
func secretishHeader(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "auth") || strings.Contains(k, "key") ||
		strings.Contains(k, "token") || strings.Contains(k, "secret") ||
		strings.Contains(k, "cookie") || strings.Contains(k, "credential")
}

// maskedConfig returns a copy of cfg safe to serialize: every
// secret-bearing field is replaced with a length mask. Read-only shared
// maps/slices (combos, extra headers that stay, aliases) are shared by
// reference and never mutated — a reload swaps whole snapshots, never
// edits a live cfg.
func maskedConfig(cfg *config.Config) *config.Config {
	m := *cfg
	m.Server.AdminPassword = maskSecret(cfg.Server.AdminPassword)
	m.Auth.KeyList = make([]config.AuthKey, len(cfg.Auth.KeyList))
	for i, k := range cfg.Auth.KeyList {
		m.Auth.KeyList[i] = config.AuthKey{Key: maskSecret(k.Key), Name: k.Name}
	}
	m.Providers = make([]config.ProviderCfg, len(cfg.Providers))
	for i, p := range cfg.Providers {
		p.APIKey = maskSecret(p.APIKey)
		if p.ExtraHeader != nil {
			h := make(map[string]string, len(p.ExtraHeader))
			for k, v := range p.ExtraHeader {
				if secretishHeader(k) {
					v = maskSecret(v)
				}
				h[k] = v
			}
			p.ExtraHeader = h
		}
		p.Accounts = make([]config.Acct, len(p.Accounts))
		for j, a := range p.Accounts {
			a.APIKey = maskSecret(a.APIKey)
			p.Accounts[j] = a
		}
		m.Providers[i] = p
	}
	return &m
}

// ---------------------------------------------------------------------------
// PUT /admin/config/reload — the SIGHUP path over HTTP
// ---------------------------------------------------------------------------

func (s *Server) handleAdminConfigReload(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	path := s.configPath()
	if path == "" {
		adminError(w, http.StatusBadRequest, "config file path unknown (start with -config or set ONEGW_CONFIG)")
		return
	}
	fresh, err := config.Load(path)
	if err != nil {
		// Same contract as SIGHUP: a bad file is rejected and the previous
		// config keeps serving.
		adminError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Reload(fresh)
	log.Printf("admin: config reloaded from %s: %d providers, %d combos, %d auth keys",
		path, len(fresh.Providers), len(fresh.Combos), len(fresh.Auth.KeyList))
	writeJSON(w, map[string]any{
		"reloaded":    true,
		"config_file": path,
		"providers":   len(fresh.Providers),
		"combos":      len(fresh.Combos),
		"auth_keys":   len(fresh.Auth.KeyList),
	})
}

// ---------------------------------------------------------------------------
// PATCH /admin/config/keys and /admin/config/aliases — file surgery
// ---------------------------------------------------------------------------

type keysPatchReq struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req keysPatchReq
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Add) == 0 && len(req.Remove) == 0 {
		adminError(w, http.StatusBadRequest, "add and remove are both empty")
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	var keys []string
	var added, removed int
	fresh, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		var out []string
		out, keys, added, removed, err = spliceAuthKeys(lines, req.Add, req.Remove)
		return out, err
	})
	if err != nil {
		editFailed(w, err)
		return
	}
	log.Printf("admin: auth keys changed: +%d -%d (total %d)", added, removed, len(keys))
	writeJSON(w, map[string]any{"added": added, "removed": removed, "total": len(keys)})
	_ = fresh
}

func (s *Server) handleAdminAliases(w http.ResponseWriter, r *http.Request) {
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
		Set    map[string]string `json:"set"`
		Delete []string          `json:"delete"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Set) == 0 && len(req.Delete) == 0 {
		adminError(w, http.StatusBadRequest, "set and delete are both empty")
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	var total, setN, delN int
	fresh, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		var out []string
		out, total, setN, delN, err = spliceAliases(lines, req.Set, req.Delete)
		return out, err
	})
	if err != nil {
		editFailed(w, err)
		return
	}
	log.Printf("admin: aliases changed: set %d, deleted %d (total %d)", setN, delN, total)
	writeJSON(w, map[string]any{"set": setN, "deleted": delN, "total": total})
	_ = fresh
}

func editFailed(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var bad badConfigEdit
	if errors.As(err, &bad) {
		status = http.StatusBadRequest
	}
	adminError(w, status, err.Error())
}

// patchConfigFile runs one read-modify-write cycle on the TOML file:
// mutate the lines, round-trip check + atomic write, then Load+Reload —
// and the caller gets the fresh config back (nil error) to hand onward.
func (s *Server) patchConfigFile(mutate func(lines []string) ([]string, error)) (*config.Config, error) {
	path := s.configPath()
	if path == "" {
		return nil, badConfigEdit{errors.New("config file path unknown (start with -config or set ONEGW_CONFIG)")}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	lines, err := mutate(strings.Split(string(raw), "\n"))
	if err != nil {
		return nil, badConfigEdit{err}
	}
	if err := writeConfigAtomically(path, []byte(strings.Join(lines, "\n"))); err != nil {
		return nil, err
	}
	fresh, err := config.Load(path)
	if err != nil {
		// Unreachable: writeConfigAtomically validated this exact content
		// with config.Load before the rename — refuse to continue on a
		// surprise rather than reload half-baked state.
		return nil, fmt.Errorf("post-write load of %s failed: %w", path, err)
	}
	s.Reload(fresh)
	return fresh, nil
}

// writeConfigAtomically replaces path's content with edited — but only
// after the edited bytes pass config.Load (parse + validate). The write is
// temp-file + rename in the same directory with the original mode; on any
// failure the original file is untouched.
func writeConfigAtomically(path string, edited []byte) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".onegw-config-*.toml")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if _, err := tmp.Write(edited); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := config.Load(name); err != nil {
		return fmt.Errorf("edited config rejected, file untouched: %w", err)
	}
	return os.Rename(name, path)
}

// ---------------------------------------------------------------------------
// TOML line splicing
// ---------------------------------------------------------------------------

var (
	sectionRe     = regexp.MustCompile(`^\s*\[+\s*([A-Za-z0-9_.-]+)\s*\]+\s*(?:#.*)?$`)
	keysAssignRe  = regexp.MustCompile(`^\s*keys\s*=`)
	aliasAssignRe = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=\s*"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$`)
	bareNameRe    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// findSection locates the `[name]` (or `[[name]]`) header and returns the
// header line index plus the exclusive end index (the next section header
// or len(lines)). Comment lines never match: headers must start with '['.
func findSection(lines []string, name string) (hdr, end int, ok bool) {
	for i, ln := range lines {
		m := sectionRe.FindStringSubmatch(ln)
		if m == nil || m[1] != name {
			continue
		}
		hdr = i
		for j := i + 1; j < len(lines); j++ {
			if sectionRe.MatchString(lines[j]) {
				return hdr, j, true
			}
		}
		return hdr, len(lines), true
	}
	return 0, 0, false
}

// spliceAuthKeys rewrites the keys array of the [auth] table: removes then
// adds are applied to the current key set (order-preserving, deduped) and
// the result is written back as a single-line basic-string array in the
// same position. Every other line — including comments inside [auth] — is
// returned untouched. Refuses (error) rather than guesses: unterminated
// arrays, non-string elements, empty or whitespace-padded keys. Removing
// the last key is refused — that would silently turn the gateway
// unauthenticated. Key material never appears in error text.
func spliceAuthKeys(lines []string, add, remove []string) (out, keys []string, added, removed int, err error) {
	for i, k := range add {
		if k == "" || k != strings.TrimSpace(k) || strings.ContainsFunc(k, unicode.IsControl) {
			return nil, nil, 0, 0, fmt.Errorf(
				"invalid key at add position %d: empty, whitespace-padded, or contains control characters", i+1)
		}
	}
	removeSet := make(map[string]bool, len(remove))
	for _, k := range remove {
		removeSet[k] = true
	}

	var cur []string
	repStart, repEnd := -1, -1
	hdr, end, found := findSection(lines, "auth")
	if found {
		for i := hdr + 1; i < end; i++ {
			if !keysAssignRe.MatchString(lines[i]) {
				continue
			}
			// Gather the (possibly multi-line) array text until it closes.
			// The scan starts at the value (after '='), not at the key.
			var arr strings.Builder
			arr.WriteString(lines[i][strings.Index(lines[i], "=")+1:])
			arr.WriteString("\n")
			j := i
			closed := false
			for {
				if items, ok := scanStringArray(arr.String()); ok {
					cur = items
					closed = true
					break
				}
				j++
				if j >= end {
					break
				}
				arr.WriteString(lines[j])
				arr.WriteString("\n")
			}
			if !closed {
				return nil, nil, 0, 0, fmt.Errorf("[auth] keys array is unterminated or has unsupported content; refusing to edit")
			}
			repStart, repEnd = i, j
			break
		}
	}

	next := make([]string, 0, len(cur)+len(add))
	seen := make(map[string]bool, len(cur)+len(add))
	for _, k := range cur {
		if removeSet[k] {
			removed++
			continue
		}
		if !seen[k] {
			seen[k] = true
			next = append(next, k)
		}
	}
	for _, k := range add {
		if seen[k] {
			continue
		}
		seen[k] = true
		next = append(next, k)
		added++
	}
	if len(next) == 0 && len(cur) > 0 {
		return nil, nil, 0, 0, fmt.Errorf("refusing to remove the last auth key (gateway would become unauthenticated)")
	}

	out = make([]string, len(lines), len(lines)+4)
	copy(out, lines)
	newLine := "keys = " + renderStringArray(next)
	switch {
	case repStart >= 0:
		out = append(out[:repStart], append([]string{newLine}, out[repEnd+1:]...)...)
	case found:
		// Section exists without a keys line: add the line at the end of
		// the table (before the next section header / trailing newline).
		at := end
		if at == len(out) && len(out) > 0 && out[at-1] == "" {
			at-- // insert before the trailing-newline element
		}
		out = append(out[:at], append([]string{newLine}, out[at:]...)...)
		if out[len(out)-1] != "" {
			out = append(out, "")
		}
	default:
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, "[auth]", newLine)
		if out[len(out)-1] != "" {
			out = append(out, "")
		}
	}
	return out, next, added, removed, nil
}

// spliceAliases sets/deletes entries of the [aliases] table (alias name →
// "provider/model", combo name, or another alias). Existing entry lines
// are replaced in place; new entries are appended at the end of the table;
// deletes remove the line. A missing [aliases] table is appended when
// there is something to set. Every other line is returned untouched.
func spliceAliases(lines []string, set map[string]string, del []string) (out []string, total, setN, delN int, err error) {
	for name, target := range set {
		if !bareNameRe.MatchString(name) {
			return nil, 0, 0, 0, fmt.Errorf("invalid alias name %q: must match [A-Za-z0-9_-]+", name)
		}
		if target == "" || strings.ContainsFunc(target, unicode.IsControl) {
			return nil, 0, 0, 0, fmt.Errorf("invalid alias target for %q: empty or contains control characters", name)
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic file output

	hdr, end, found := findSection(lines, "aliases")
	out = make([]string, len(lines), len(lines)+len(names)+4)
	copy(out, lines)

	// Existing entries: name → line index.
	existing := map[string]int{}
	if found {
		for i := hdr + 1; i < end; i++ {
			if m := aliasAssignRe.FindStringSubmatch(lines[i]); m != nil {
				existing[m[1]] = i
			}
		}
	}

	// Deletes: skip names also present in set (set wins).
	delSet := make(map[string]bool, len(del))
	for _, d := range del {
		if _, inSet := set[d]; inSet {
			continue
		}
		delSet[d] = true
	}
	delIdx := map[int]bool{}
	for name := range delSet {
		if idx, ok := existing[name]; ok {
			delIdx[idx] = true
			delN++
		}
	}

	// Sets: replace in place, or queue for append at the section tail.
	var toAppend []string
	for _, name := range names {
		line := name + " = " + escapeTOMLBasic(set[name])
		if _, ok := existing[name]; ok {
			out[existing[name]] = line
		} else {
			toAppend = append(toAppend, line)
			total++
		}
		setN++
	}
	total += len(existing) - delN

	var final []string
	inserted := false
	for i := 0; i < len(out); i++ {
		if delIdx[i] {
			continue
		}
		if found && i == end && len(toAppend) > 0 && !inserted {
			final = append(final, toAppend...)
			inserted = true
		}
		final = append(final, out[i])
	}
	switch {
	case inserted:
	case found && len(toAppend) > 0: // section ran to EOF
		at := len(final)
		if at > 0 && final[at-1] == "" {
			at-- // before the trailing-newline element
		}
		final = append(final[:at], append(append([]string{}, toAppend...), "")...)
	case !found && len(toAppend) > 0:
		if len(final) > 0 && final[len(final)-1] != "" {
			final = append(final, "")
		}
		final = append(final, "[aliases]")
		final = append(final, toAppend...)
		if final[len(final)-1] != "" {
			final = append(final, "")
		}
	}
	return final, total, setN, delN, nil
}

// renderStringArray writes a single-line TOML string array.
func renderStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, k := range items {
		parts[i] = escapeTOMLBasic(k)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// scanStringArray parses a TOML string array from text (which begins at or
// before the opening bracket). Elements must be quoted strings; basic-string
// escapes are unescaped. It returns ok=false for anything it cannot parse
// conservatively — unterminated arrays, nested arrays, non-string elements —
// so callers refuse the edit instead of corrupting the file.
func scanStringArray(text string) (items []string, ok bool) {
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	if i >= len(text) || text[i] != '[' {
		return nil, false
	}
	i++
	depth, quote, esc := 1, byte(0), false
	start := -1
	var elems []string
	closed := false
scan:
	for ; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			switch {
			case quote == '"' && esc:
				esc = false
			case quote == '"' && c == '\\':
				esc = true
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			if depth == 1 && start < 0 {
				start = i
			}
			quote = c
		case '[':
			return nil, false // nested arrays unsupported — refuse
		case ']':
			depth--
			if depth == 0 {
				if start >= 0 {
					elems = append(elems, text[start:i])
				}
				closed = true
				break scan
			}
		case ',':
			if depth == 1 {
				if start >= 0 {
					elems = append(elems, text[start:i])
				}
				start = -1
			}
		case '#':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		default:
			if depth == 1 && start < 0 && c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				return nil, false // non-string element — refuse the edit
			}
		}
	}
	if !closed {
		return nil, false
	}
	for _, e := range elems {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		s, good := parseTOMLString(e)
		if !good {
			return nil, false
		}
		items = append(items, s)
	}
	return items, true
}

// parseTOMLString parses exactly one quoted TOML string (basic "..." or
// literal '...'), allowing trailing spaces and a trailing comment.
func parseTOMLString(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	q := s[0]
	if q != '"' && q != '\'' {
		return "", false
	}
	i := 1
	for ; i < len(s); i++ {
		c := s[i]
		if q == '"' && c == '\\' {
			i++
			continue
		}
		if c == q {
			break
		}
	}
	if i >= len(s) || s[i] != q {
		return "", false
	}
	rest := strings.TrimSpace(s[i+1:])
	if rest != "" && rest[0] != '#' {
		return "", false
	}
	inner := s[1:i]
	if q == '\'' {
		return inner, true // literal strings have no escapes
	}
	return unescapeTOMLBasic(inner)
}

func unescapeTOMLBasic(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", false
		}
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'u', 'U':
			n := 4
			if s[i] == 'U' {
				n = 8
			}
			if i+n >= len(s) {
				return "", false
			}
			v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil {
				return "", false
			}
			b.WriteRune(rune(v))
			i += n
		default:
			return "", false
		}
	}
	return b.String(), true
}

func escapeTOMLBasic(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
