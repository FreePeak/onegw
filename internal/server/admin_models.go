package server

// Model discovery: ask a provider what it actually serves, and let the
// operator turn that answer into config.
//
// The gateway routes a model it has never heard of (pass-through), so an
// upstream catalog is not required — it is what makes the console usable: the
// id to copy into a CLI, the list to pick a combo leg from, the evidence that
// a key is live. Two ways in, one code path:
//
//	POST /admin/config/providers/{name}/models/fetch  {"apply":bool}
//	GET  /admin/api/v1/models             — the page's rows
//
// `apply:true` also writes the answer into the provider's `models` line, which
// is a real behaviour change (an empty list means pass-through, so pinning one
// restricts what routes there). It is therefore never implicit: the auto pass
// fills the cache, the button in the UI asks, and only Save writes.
//
// The cache lives on the Server, not the reloadable state: a SIGHUP that swaps
// a provider's key must not throw away a catalog the operator is looking at.
// Failed fetches are cached too (with their error) so an open dashboard does
// not re-probe a dead upstream on every poll.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"onegw/internal/config"
	"onegw/internal/provider"
)

// discoveryTTL is how long a cached catalog is trusted before the dashboard
// re-probes. Longer than any operator glances at the page, shorter than a
// vendor's model rollout.
const discoveryTTL = 6 * time.Hour

// discoveryTimeout bounds one upstream catalog request. Generous: a big
// relay's /v1/models carries thousands of entries.
const discoveryTimeout = 15 * time.Second

// discoveryConcurrency is how many providers are probed at once by the auto
// pass. Three covers the common case (one slow vendor should not serialize the
// whole page) and keeps a boot-time fan-out off the gateway's latency budget.
const discoveryConcurrency = 3

// discoveredModels is one provider's catalog, as last seen upstream.
type discoveredModels struct {
	IDs   []string `json:"ids"`
	At    string   `json:"at,omitempty"` // RFC 3339
	Error string   `json:"error,omitempty"`
}

// modelCache is the Server-side discovery cache.
type modelCache struct {
	mu      sync.Mutex
	byProv  map[string]discoveredModels
	probing map[string]bool // a probe is running (dedupes concurrent page loads)
}

func newModelCache() *modelCache {
	return &modelCache{byProv: map[string]discoveredModels{}, probing: map[string]bool{}}
}

func (c *modelCache) get(name string) (discoveredModels, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.byProv[name]
	return d, ok
}

func (c *modelCache) put(name string, d discoveredModels) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byProv[name] = d
}

// claim marks a provider as being probed; false means one already is.
func (c *modelCache) claim(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probing[name] {
		return false
	}
	c.probing[name] = true
	return true
}

// busy reports whether a probe is in flight for name.
func (c *modelCache) busy(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probing[name]
}

func (c *modelCache) release(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.probing, name)
}

// modelRowView is one provider's row on the models page.
type modelRowView struct {
	Provider    string   `json:"provider"`
	Kind        string   `json:"kind"`
	Disabled    bool     `json:"disabled"`
	Configured  []string `json:"configured"`
	Discovered  []string `json:"discovered,omitempty"`
	FetchedAt   string   `json:"fetched_at,omitempty"`
	Error       string   `json:"error,omitempty"`
	Pending     bool     `json:"pending,omitempty"` // a probe is in flight
	Passthrough bool     `json:"passthrough"`       // no configured list: any id routes here
	// ShowOpen marks a card whose id lists are short enough to be the answer
	// you came for: below copyableOpenAt they render unfolded, above it the
	// list IS the long config and collapses like everywhere else.
	ShowOpen bool `json:"show_open,omitempty"`
}

// copyableOpenAt is the id count at or below which a models card starts open.
const copyableOpenAt = 8

// handleAPIModels lists advertised vs discovered models per provider. Cheap by
// design (no upstream calls): it reads config + cache, so the dashboard can
// poll it. `?refresh=1` asks for a background re-probe of every provider that
// has no fresh answer yet — which is what makes the flow "auto".
func (s *Server) handleAPIModels(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	st := s.cur()
	if st == nil {
		writeJSON(w, []modelRowView{})
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		s.autoDiscoverModels(st, false)
	}
	writeJSON(w, s.modelRows(st))
}

// modelRows pairs each provider's advertised list with the cached upstream
// catalog. Pure read: no network, so both the page and the JSON twin can use it
// on every poll.
func (s *Server) modelRows(st *state) []modelRowView {
	if st == nil {
		return []modelRowView{}
	}
	rows := make([]modelRowView, 0, len(st.cfg.Providers))
	for _, p := range st.cfg.Providers {
		if p.Kind == "searxng" {
			continue // a search surface, not a model catalog
		}
		row := modelRowView{Provider: p.Name, Kind: p.Kind, Disabled: p.Disabled, Configured: []string{}}
		if def, ok := st.pool.Get(p.Name); ok {
			row.Configured = def.Models
		}
		row.Passthrough = len(row.Configured) == 0
		shown := len(row.Configured)
		if d, ok := s.models.get(p.Name); ok && len(d.IDs) > shown {
			shown = len(d.IDs)
		}
		row.ShowOpen = shown > 0 && shown <= copyableOpenAt
		if d, ok := s.models.get(p.Name); ok {
			row.Discovered, row.FetchedAt, row.Error = d.IDs, d.At, d.Error
		}
		if s.models.busy(p.Name) {
			row.Pending = true
		}
		rows = append(rows, row)
	}
	return rows
}

// handleAdminModelFetch probes one provider's upstream catalog.
//
// Body: {"apply": true} also pins the answer into the provider's `models`
// line (validated, atomic write, hot reload — like every other config edit).
// Without it the answer only lands in the cache, which is what the page's
// "Fetch" button does; "Save" is the explicit apply.
func (s *Server) handleAdminModelFetch(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	name := r.PathValue("name")
	st := s.cur()
	if st == nil {
		adminError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	def, ok := st.pool.Get(name)
	if !ok {
		adminError(w, http.StatusNotFound, "no provider "+name)
		return
	}
	var req struct {
		Apply bool `json:"apply"`
	}
	if body, err := s.readBody(r); err == nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), discoveryTimeout)
	defer cancel()
	ids, acctErr := fetchProviderModels(ctx, def)
	if len(ids) == 0 {
		msg := acctErr
		if msg == "" {
			msg = "the upstream returned no models"
		}
		s.models.put(name, discoveredModels{At: time.Now().Format(time.RFC3339), Error: msg})
		adminError(w, http.StatusBadGateway, msg)
		return
	}
	s.models.put(name, discoveredModels{IDs: ids, At: time.Now().Format(time.RFC3339)})

	resp := map[string]any{"provider": name, "models": ids, "count": len(ids), "applied": false}
	if req.Apply {
		s.cfgMu.Lock()
		err := s.pinModels(name, ids)
		s.cfgMu.Unlock()
		if err != nil {
			adminError(w, http.StatusBadRequest, "fetched "+fmt.Sprint(len(ids))+" models, but saving them failed: "+err.Error())
			return
		}
		resp["applied"] = true
	}
	log.Printf("admin: provider %s catalog fetched: %d models (applied=%v)", name, len(ids), resp["applied"])
	s.events.publish("config", `{"models":`+jsonString(name)+`}`)
	writeJSON(w, resp)
}

// pinModels writes a freshly discovered list into one provider block's
// `models` line through the provider editor's own splice, so every other field
// of the block is carried over byte-for-byte.
func (s *Server) pinModels(name string, ids []string) error {
	st := s.cur()
	if st == nil {
		return fmt.Errorf("no config loaded")
	}
	var pc *config.ProviderCfg
	for i := range st.cfg.Providers {
		if st.cfg.Providers[i].Name == name {
			pc = &st.cfg.Providers[i]
			break
		}
	}
	if pc == nil {
		return fmt.Errorf("not configured")
	}
	before := strings.Join(pc.Models, ",")
	req := providerEditReqFromConfig(*pc, st.cfg.OAuthAccounts())
	req.Models = ids
	_, err := s.patchConfigFile(func(lines []string) ([]string, error) {
		out, _, err := spliceProvider(lines, req)
		return out, err
	})
	if err == nil {
		log.Printf("admin: provider %s models pinned to %d discovered ids (was %q)", name, len(ids), before)
	}
	return err
}

// autoDiscoverModels probes providers whose catalog the dashboard has no
// answer for. Only ever launched from a page load or an explicit refresh, so
// an idle gateway makes no upstream requests; `force` re-probes everything.
func (s *Server) autoDiscoverModels(st *state, force bool) {
	var need []string
	for _, p := range st.cfg.Providers {
		if p.Kind == "searxng" {
			continue
		}
		d, ok := s.models.get(p.Name)
		if ok && !force && (d.Error == "" && time.Since(parseOrZero(d.At)) < discoveryTTL) {
			continue
		}
		if ok && !force && d.Error != "" && time.Since(parseOrZero(d.At)) < 5*time.Minute {
			continue // back off briefly on a dead upstream, then try again
		}
		need = append(need, p.Name)
	}
	if len(need) == 0 {
		return
	}
	go func() {
		sem := make(chan struct{}, discoveryConcurrency)
		var wg sync.WaitGroup
		for _, name := range need {
			if !s.models.claim(name) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(name string) {
				defer wg.Done()
				defer func() { <-sem }()
				defer s.models.release(name)
				s.probeProvider(name)
			}(name)
		}
		wg.Wait()
	}()
}

// probeProvider fetches one provider's catalog into the cache, recording the
// failure rather than hiding it (the page shows why a list is empty).
func (s *Server) probeProvider(name string) {
	st := s.cur()
	if st == nil {
		return
	}
	def, ok := st.pool.Get(name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	ids, err := fetchProviderModels(ctx, def)
	d := discoveredModels{IDs: ids, At: time.Now().Format(time.RFC3339)}
	if len(ids) == 0 {
		d.IDs = nil
		if err == "" {
			err = "the upstream returned no models"
		}
		d.Error = err
		log.Printf("admin: model discovery for %s failed: %s", name, err)
		return
	}
	s.models.put(name, d)
}

// fetchProviderModels asks a live provider def for its catalog, trying each
// account until one answers. Providers are frequently multi-account with only
// some keys entitled to the listing endpoint.
func fetchProviderModels(ctx context.Context, def *provider.Def) ([]string, string) {
	accts := def.Accounts
	if len(accts) == 0 {
		return nil, "provider has no account to probe"
	}
	var lastErr string
	for i := range accts {
		body, status, err := def.FetchModels(ctx, &accts[i])
		if err != nil {
			lastErr = accts[i].Name + ": " + err.Error()
			continue
		}
		if status >= 400 {
			lastErr = fmt.Sprintf("%s: HTTP %d %s", accts[i].Name, status, snippet(body))
			continue
		}
		if ids := parseModelIDs(def.Kind, body); len(ids) > 0 {
			return ids, ""
		}
		lastErr = accts[i].Name + ": no models in the response"
	}
	return nil, lastErr
}

// parseModelIDs reads a catalog out of an upstream reply in whichever of the
// three shapes the configured kinds answer with: OpenAI's data[].id,
// Anthropic's same shape, or Gemini's models[].name ("models/gemini-x").
func parseModelIDs(kind provider.Kind, body []byte) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	var openai struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &openai) == nil {
		for _, m := range openai.Data {
			add(m.ID)
		}
	}
	if len(out) == 0 {
		var gemini struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.Unmarshal(body, &gemini) == nil {
			for _, m := range gemini.Models {
				add(strings.TrimPrefix(m.Name, "models/"))
			}
		}
	}
	if len(out) == 0 {
		var list struct {
			Model []string `json:"models"`
		}
		if json.Unmarshal(body, &list) == nil {
			for _, m := range list.Model {
				add(m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// snippet keeps an upstream's error body reportable without dumping a whole
// catalog (or a page of HTML) into the dashboard.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

func parseOrZero(rfc string) time.Time {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return time.Time{}
	}
	return t
}
