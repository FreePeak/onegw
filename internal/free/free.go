// Package free holds the catalog-backed model index: which models exist,
// how big their context window is, how many output tokens they may
// produce, and whether they sit on a free lane.
//
// Every number comes from live evidence — an upstream /models body or the
// models.dev catalog — never from a hand-written table. A hand-written
// declaration (a [[providers.tier]] row) always wins over a parsed row:
// the index is additive-only, so a catalog refresh can fill in what is
// unknown but never lower what the operator declared.
package free

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// MaxCatalogBytes caps a parsed catalog body (8 MiB). The models.dev
// document is ~5.9 MB today; the cap keeps a runaway or hostile upstream
// from being read into memory.
const MaxCatalogBytes = 8 << 20

// Where an entry's numbers came from.
const (
	// SourceCatalog marks a row parsed from a catalog document.
	SourceCatalog = "catalog"
	// SourceConfig marks a hand-written declaration. Static config wins.
	SourceConfig = "config"
)

// Entry is one model's catalog facts. A zero Context/MaxOut means
// "unknown" — the value is never guessed.
type Entry struct {
	// Provider is the catalog provider the row was found under (for the
	// models.dev document: the provider key, e.g. "opencode-go").
	Provider string `json:"provider"`
	// Model is the upstream model id, verbatim.
	Model string `json:"model"`
	// Free is the free-lane flag, derived from the id tail.
	Free bool `json:"free"`
	// Source is "catalog" or "config".
	Source string `json:"source"`
	// Context is the token context window (0 = unknown).
	Context int `json:"context"`
	// MaxOut is the max output tokens (0 = unknown).
	MaxOut int `json:"max_out"`
}

// Index is the in-memory catalog: provider -> model -> entry.
//
// Lookups are provider-scoped because limits are provider+model keyed:
// the same model id behind two gateways can carry two different windows.
// The empty provider is the wildcard bucket for rows that arrived without
// one (a plain /models body); a provider-scoped lookup falls back to it.
type Index struct {
	mu   sync.RWMutex
	prov map[string]map[string]Entry
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{prov: map[string]map[string]Entry{}}
}

// Upsert merges e into the index, additively.
//
// The rules, in order:
//
//   - a known (non-zero) value is never lowered: an incoming row only
//     fills in what the index does not know yet;
//   - a static config declaration (Source == SourceConfig) wins over a
//     parsed catalog row for the same provider/model;
//   - the free flag is sticky once true.
//
// Use Replace to swap a whole catalog snapshot (the sync path), which is
// the only way a catalog row's own numbers are refreshed.
func (ix *Index) Upsert(e Entry) {
	if e.Model == "" {
		return
	}
	if e.Source == "" {
		e.Source = SourceCatalog
	}
	if !e.Free {
		e.Free = IsFreeID(e.Model)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.prov == nil {
		ix.prov = map[string]map[string]Entry{}
	}
	m := ix.prov[e.Provider]
	if m == nil {
		m = map[string]Entry{}
		ix.prov[e.Provider] = m
	}
	cur, ok := m[e.Model]
	if !ok {
		m[e.Model] = e
		return
	}
	merged := cur
	if e.Free {
		merged.Free = true
	}
	if merged.Context == 0 {
		merged.Context = e.Context
	}
	if merged.MaxOut == 0 {
		merged.MaxOut = e.MaxOut
	}
	// Static config outranks a parsed catalog row.
	if e.Source == SourceConfig && cur.Source != SourceConfig {
		if e.Context > 0 {
			merged.Context = e.Context
		}
		if e.MaxOut > 0 {
			merged.MaxOut = e.MaxOut
		}
		merged.Source = SourceConfig
	}
	m[e.Model] = merged
}

// Replace swaps the catalog rows for a fresh snapshot, keeping the
// entries a static config declaration owns (those always win).
func (ix *Index) Replace(rows []Entry) {
	ix.mu.Lock()
	keep := map[string]map[string]Entry{}
	for prov, m := range ix.prov {
		for model, e := range m {
			if e.Source != SourceConfig {
				continue
			}
			if keep[prov] == nil {
				keep[prov] = map[string]Entry{}
			}
			keep[prov][model] = e
		}
	}
	ix.mu.Unlock()

	fresh := NewIndex()
	for _, e := range rows {
		fresh.Upsert(e)
	}
	// Static declarations ride along with the fresh snapshot.
	for prov, m := range keep {
		for model, e := range m {
			e.Provider, e.Model = prov, model
			fresh.Upsert(e)
		}
	}
	ix.mu.Lock()
	ix.prov = fresh.prov
	ix.mu.Unlock()
}

// Lookup returns the entry for a provider+model. The provider-scoped row
// wins; a row stored under the wildcard (empty) provider is the fallback.
func (ix *Index) Lookup(prov, model string) (Entry, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if m, ok := ix.prov[prov]; ok {
		if e, ok := m[model]; ok {
			return e, true
		}
	}
	if m, ok := ix.prov[""]; ok {
		if e, ok := m[model]; ok {
			return e, true
		}
	}
	return Entry{}, false
}

// Len reports the number of indexed rows.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := 0
	for _, m := range ix.prov {
		n += len(m)
	}
	return n
}

// Providers lists the indexed catalog providers, sorted.
func (ix *Index) Providers() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.prov))
	for p := range ix.prov {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// IsFreeID reports whether a model id is a free variant: the ":free",
// "-free" or "/free" tail (the three spellings the upstreams use).
func IsFreeID(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, suf := range []string{":free", "-free", "/free"} {
		if strings.HasSuffix(m, suf) {
			return true
		}
	}
	return false
}

// ParseModelList parses a catalog document into entries. It tolerates the
// three shapes the upstreams emit:
//
//	models.dev:  {"<provider>": {"models": {"<model>": {"limit": {"context": N, "output": N}}}}}
//	OpenRouter:  {"data": [{"id": "vendor/model", "context_length": N, "pricing": {...}}]}
//	plain:       {"data": [{"id": "model"}]} / {"models": [{"id": "model"}]}
//
// An unknown shape yields an empty slice and never an error: a catalog
// that changed shape must not break the gateway. A body over
// MaxCatalogBytes is refused the same way.
func ParseModelList(body []byte) []Entry {
	if len(body) == 0 || len(body) > MaxCatalogBytes {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	if rows, ok := parseListShape(top); ok {
		return rows
	}
	if rows, ok := parseModelsDev(top); ok {
		return rows
	}
	return nil
}

// parseListShape reads the {"data": [...]} / {"models": [...]} shapes.
func parseListShape(top map[string]json.RawMessage) ([]Entry, bool) {
	for _, key := range []string{"data", "models"} {
		raw, ok := top[key]
		if !ok {
			continue
		}
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			continue // not a list under this key
		}
		rows := make([]Entry, 0, len(list))
		for _, item := range list {
			var row listRow
			if err := json.Unmarshal(item, &row); err != nil {
				continue
			}
			id := strings.TrimSpace(row.ID)
			if id == "" {
				id = strings.TrimSpace(row.Name)
			}
			if id == "" {
				continue
			}
			prov, model := splitModelID(id)
			e := Entry{
				Provider: prov,
				Model:    model,
				Free:     IsFreeID(model),
				Source:   SourceCatalog,
				Context:  row.context(),
				MaxOut:   row.maxOut(),
			}
			rows = append(rows, e)
		}
		return rows, true
	}
	return nil, false
}

// listRow is one row of a {"data": [...]} catalog body.
type listRow struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	ContextLength      int    `json:"context_length"`
	ContextWindow      int    `json:"context_window"`
	MaxOutputTokens    int    `json:"max_output_tokens"`
	MaxCompletionToken int    `json:"max_completion_tokens"`
	TopProvider        *struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Limit *struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

func (r listRow) context() int {
	if r.Limit != nil && r.Limit.Context > 0 {
		return r.Limit.Context
	}
	if r.ContextLength > 0 {
		return r.ContextLength
	}
	return r.ContextWindow
}

func (r listRow) maxOut() int {
	if r.MaxOutputTokens > 0 {
		return r.MaxOutputTokens
	}
	if r.MaxCompletionToken > 0 {
		return r.MaxCompletionToken
	}
	if r.TopProvider != nil && r.TopProvider.MaxCompletionTokens > 0 {
		return r.TopProvider.MaxCompletionTokens
	}
	if r.Limit != nil {
		return r.Limit.Output
	}
	return 0
}

// mdProvider is one provider block of the models.dev document.
type mdProvider struct {
	Models map[string]struct {
		Limit struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		} `json:"limit"`
	} `json:"models"`
}

// parseModelsDev reads the models.dev document: a top-level provider map
// whose values carry a "models" map of model id -> {"limit": {...}}.
func parseModelsDev(top map[string]json.RawMessage) ([]Entry, bool) {
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rows []Entry
	for _, key := range keys {
		var p mdProvider
		if err := json.Unmarshal(top[key], &p); err != nil {
			continue
		}
		if len(p.Models) == 0 {
			continue
		}
		models := make([]string, 0, len(p.Models))
		for m := range p.Models {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			lim := p.Models[m].Limit
			rows = append(rows, Entry{
				Provider: key,
				Model:    m,
				Free:     IsFreeID(m),
				Source:   SourceCatalog,
				Context:  lim.Context,
				MaxOut:   lim.Output,
			})
		}
	}
	return rows, len(rows) > 0
}

// splitModelID splits "vendor/model" into its two halves. A slash-less id
// lands in the wildcard provider bucket.
func splitModelID(id string) (prov, model string) {
	if i := strings.Index(id, "/"); i > 0 && i < len(id)-1 {
		return id[:i], id[i+1:]
	}
	return "", id
}
