package server

// Shared proxy-pool admin surface: one place to bulk-add proxy URLs,
// pick the rotation strategy, and multi-select which providers route
// through the pool — no navigation to the provider editor needed.
//
//	GET /admin/config/proxies — the live pool (urls, no_proxy,
//	  rotation) plus, per provider, name + current proxy opt-in.
//	PUT /admin/config/proxies — replace the [proxy] table and set each
//	  named provider's `proxy =` opt-in in one save.
//
// Follows the admin_config.go contract: the TOML file stays the single
// source of truth, mutations are line-based splices (comments and
// untouched sections preserved byte-for-byte), the edited content is
// validated with config.Load BEFORE it touches disk (400 on any
// validation error, original file untouched), the write is atomic, and
// the new config is applied through the same Load+Reload path as SIGHUP.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// proxyPoolView is the GET shape: the pool plus every provider's opt-in.
type proxyPoolView struct {
	URLs      []string          `json:"urls"`
	NoProxy   string            `json:"no_proxy,omitempty"`
	Rotation  string            `json:"rotation,omitempty"`
	Providers []proxyMemberView `json:"providers"`
}

// proxyMemberView is one provider's row on the Proxies page: its name
// and whether it currently routes through the pool.
type proxyMemberView struct {
	Name  string `json:"name"`
	Proxy bool   `json:"proxy"`
}

// proxyPoolEdit is the PUT shape: the whole pool (bulk URLs, bypass,
// strategy) plus the full provider opt-in set. Providers lists EVERY
// provider shown with its desired opt-in; omitted names keep their
// on-disk value so a stale page never mass-disables.
type proxyPoolEdit struct {
	URLs      []string         `json:"urls"`
	NoProxy   string           `json:"no_proxy"`
	Rotation  string           `json:"rotation"`
	Providers []proxyMemberSet `json:"providers"`
}

// proxyMemberSet is one provider's desired opt-in in the PUT body.
type proxyMemberSet struct {
	Name  string `json:"name"`
	Proxy bool   `json:"proxy"`
}

func (s *Server) handleAdminProxiesGet(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	st := s.cur()
	v := proxyPoolView{}
	if st != nil {
		v.URLs = append([]string{}, st.cfg.Proxy.URLs...)
		v.NoProxy = st.cfg.Proxy.NoProxy
		v.Rotation = st.cfg.Proxy.Rotation
		for _, p := range st.cfg.Providers {
			v.Providers = append(v.Providers, proxyMemberView{Name: p.Name, Proxy: p.Proxy})
		}
		sort.Slice(v.Providers, func(i, j int) bool { return v.Providers[i].Name < v.Providers[j].Name })
	}
	writeJSON(w, v)
}

func (s *Server) handleAdminProxiesPut(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req proxyPoolEdit
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	if _, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		out := spliceProxyPool(lines, req)
		want := map[string]bool{}
		for _, m := range req.Providers {
			want[m.Name] = m.Proxy
		}
		// Only touch providers the request names: anything else (a
		// provider added elsewhere since the page loaded) keeps its
		// on-disk value. Collect edits first, then apply from the END of
		// the file backwards so earlier block ranges stay valid.
		type blockEdit struct {
			b     tomlBlock
			proxy bool
		}
		var edits []blockEdit
		for _, b := range scanBlocks(out, "[[providers]]") {
			name, ok := blockName(out, b)
			if !ok {
				continue
			}
			proxy, named := want[name]
			if !named {
				continue
			}
			edits = append(edits, blockEdit{b, proxy})
		}
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			edited := cloneLines(out[e.b.start:e.b.end])
			if e.proxy {
				edited = upsertScalar(edited, "proxy", "proxy = true")
			} else {
				edited = removeScalar(edited, "proxy")
			}
			out = append(cloneLines(out[:e.b.start]), append(edited, out[e.b.end:]...)...)
		}
		// Unknown names are a client bug, not a silent skip.
		known := map[string]bool{}
		for _, b := range scanBlocks(out, "[[providers]]") {
			if name, ok := blockName(out, b); ok {
				known[name] = true
			}
		}
		for name := range want {
			if !known[name] {
				return nil, fmt.Errorf("unknown provider %q", name)
			}
		}
		if err := validateLines(out); err != nil {
			return nil, err
		}
		return out, nil
	}); err != nil {
		editFailed(w, err)
		return
	}
	s.events.publish("config", `{"proxy_pool":true}`)
	log.Printf("admin: proxy pool saved (%d urls, rotation %q)", len(req.URLs), poolRotationLabel(req.Rotation))
	writeJSON(w, map[string]any{"urls": len(req.URLs), "rotation": poolRotationLabel(req.Rotation), "reload": true})
}

func poolRotationLabel(rot string) string {
	if s := strings.ToLower(strings.TrimSpace(rot)); s != "" {
		return s
	}
	return "round-robin"
}

// spliceProxyPool rewrites the top-level [proxy] table: urls/no_proxy/
// rotation lines replaced in place when the table exists, the table
// appended when missing, the table removed when the request carries no
// urls. Every other line is returned untouched.
func spliceProxyPool(lines []string, req proxyPoolEdit) []string {
	hdr, end, found := findSection(lines, "proxy")
	if len(req.URLs) == 0 {
		if !found {
			return lines
		}
		out := append(cloneLines(lines[:hdr]), lines[end:]...)
		return out
	}
	var table []string
	table = append(table, "[proxy]")
	table = append(table, "urls = "+renderStringArray(req.URLs))
	if strings.TrimSpace(req.NoProxy) != "" {
		table = append(table, "no_proxy = "+escapeTOMLBasic(strings.TrimSpace(req.NoProxy)))
	}
	if strings.TrimSpace(req.Rotation) != "" {
		table = append(table, "rotation = "+escapeTOMLBasic(poolRotationLabel(req.Rotation)))
	}
	if !found {
		out := cloneLines(lines)
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "")
		out = append(out, table...)
		return out
	}
	out := append(cloneLines(lines[:hdr]), table...)
	return append(out, lines[end:]...)
}
