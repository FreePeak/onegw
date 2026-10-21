package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"onegw/internal/config"
	"onegw/internal/free"
)

// limitsCfg is the live-shaped config: an opencode subscription lane, an
// opencode-free lane, one combo and one alias.
func limitsCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Server.AdminPassword = "pw"
	cfg.Providers = []config.ProviderCfg{
		{
			Name: "opencode", Kind: "opencode", BaseURL: "https://opencode.ai/zen/go",
			APIKey: "subscription-key",
			Models: []string{"minimax-m3.1-flash-free", "unknown-model"},
			Tiers: []config.TierCfg{
				// A hand-written declaration wins over the catalog.
				{Model: "minimax-m3.1-flash-free", Context: 4096},
			},
		},
		{
			Name: "opencode-free", Kind: "opencode-free", BaseURL: "https://opencode.ai/zen/v1",
			Models: []string{"deepseek-v4.1-flash-free"},
		},
	}
	cfg.Combos = []config.ComboCfg{
		{Name: "execution", Targets: []string{"opencode/minimax-m3.1-flash-free", "opencode-free/deepseek-v4.1-flash-free"}},
		{Name: "partial", Targets: []string{"opencode/minimax-m3.1-flash-free", "opencode/unknown-model"}},
	}
	cfg.Aliases = map[string]string{"fast": "opencode/minimax-m3.1-flash-free"}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

// modelsJSON fetches /v1/models and returns the rows by id.
func modelsJSON(t *testing.T, srv *Server) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := do(t, srv.Handler(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200", w.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []map[string]any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	if out.Object != "list" {
		t.Fatalf("object = %q, want list", out.Object)
	}
	rows := map[string]map[string]any{}
	for _, row := range out.Data {
		id, _ := row["id"].(string)
		rows[id] = row
	}
	return rows
}

// TestModelsAdvertiseCatalogLimits: the limits on /v1/models come from
// the live catalog, a hand-written tier wins over it, and an unknown
// model carries no limit key at all.
func TestModelsAdvertiseCatalogLimits(t *testing.T) {
	srv, err := New(limitsCfg())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	// The live catalog: the opencode subscription lane reads the
	// models.dev "opencode-go" entry, the free lane reads "opencode".
	srv.freeIdx.Upsert(free.Entry{Provider: "opencode-go", Model: "minimax-m3.1-flash-free", Source: free.SourceCatalog, Context: 1000000, MaxOut: 512000})
	srv.freeIdx.Upsert(free.Entry{Provider: "opencode", Model: "deepseek-v4.1-flash-free", Source: free.SourceCatalog, Context: 200000, MaxOut: 32000})

	rows := modelsJSON(t, srv)

	// The tier declaration (4096) wins over the catalog (1000000); the
	// output cap still comes from the catalog.
	row, ok := rows["opencode/minimax-m3.1-flash-free"]
	if !ok {
		t.Fatal("opencode/minimax-m3.1-flash-free not advertised")
	}
	if got := row["context_length"]; got != float64(4096) {
		t.Fatalf("context_length = %v, want the declared 4096", got)
	}
	if got := row["max_output_tokens"]; got != float64(512000) {
		t.Fatalf("max_output_tokens = %v, want 512000", got)
	}
	if row["object"] != "model" || row["owned_by"] != "onegw" {
		t.Fatalf("row shape changed: %+v", row)
	}

	// The free lane reads the models.dev "opencode" entry.
	row, ok = rows["opencode-free/deepseek-v4.1-flash-free"]
	if !ok {
		t.Fatal("opencode-free/deepseek-v4.1-flash-free not advertised")
	}
	if got := row["context_length"]; got != float64(200000) {
		t.Fatalf("context_length = %v, want 200000", got)
	}
	if got := row["max_output_tokens"]; got != float64(32000) {
		t.Fatalf("max_output_tokens = %v, want 32000", got)
	}

	// An unknown model carries no limit key at all.
	row, ok = rows["opencode/unknown-model"]
	if !ok {
		t.Fatal("opencode/unknown-model not advertised")
	}
	if _, has := row["context_length"]; has {
		t.Fatalf("unknown row carries a context_length: %+v", row)
	}
	if _, has := row["max_output_tokens"]; has {
		t.Fatalf("unknown row carries a max_output_tokens: %+v", row)
	}

	// A combo takes the minimum across its legs.
	row, ok = rows["execution"]
	if !ok {
		t.Fatal("execution not advertised")
	}
	if got := row["context_length"]; got != float64(4096) {
		t.Fatalf("combo context_length = %v, want the leg minimum 4096", got)
	}
	if got := row["max_output_tokens"]; got != float64(32000) {
		t.Fatalf("combo max_output_tokens = %v, want the leg minimum 32000", got)
	}

	// A combo with an unknown leg carries no limit: a chain is only as
	// good as its worst leg.
	row, ok = rows["partial"]
	if !ok {
		t.Fatal("partial not advertised")
	}
	if _, has := row["context_length"]; has {
		t.Fatalf("combo with an unknown leg carries a context_length: %+v", row)
	}
	if _, has := row["max_output_tokens"]; has {
		t.Fatalf("combo with an unknown leg carries a max_output_tokens: %+v", row)
	}

	// An alias inherits what it points at.
	row, ok = rows["fast"]
	if !ok {
		t.Fatal("fast alias not advertised")
	}
	if got := row["context_length"]; got != float64(4096) {
		t.Fatalf("alias context_length = %v, want 4096", got)
	}
	if got := row["max_output_tokens"]; got != float64(512000) {
		t.Fatalf("alias max_output_tokens = %v, want 512000", got)
	}
}

// TestModelsAdvertiseDeclaredComboLimits: a combo may declare its own
// window, and the declaration wins over the derived per-leg minimum —
// that is how an operator states what the chain as a whole can carry
// (a client must not be told a number the gateway cannot honour).
func TestModelsAdvertiseDeclaredComboLimits(t *testing.T) {
	cfg := limitsCfg()
	// "execution" spans a 4096 leg and a 200000 leg; the operator
	// declares the chain carries 500000.
	for i := range cfg.Combos {
		if cfg.Combos[i].Name == "execution" {
			cfg.Combos[i].Context = 500000
			cfg.Combos[i].MaxOut = 128000
		}
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	srv.freeIdx.Upsert(free.Entry{Provider: "opencode-go", Model: "minimax-m3.1-flash-free", Source: free.SourceCatalog, Context: 1000000, MaxOut: 512000})
	srv.freeIdx.Upsert(free.Entry{Provider: "opencode", Model: "deepseek-v4.1-flash-free", Source: free.SourceCatalog, Context: 200000, MaxOut: 32000})

	rows := modelsJSON(t, srv)
	row, ok := rows["execution"]
	if !ok {
		t.Fatal("execution not advertised")
	}
	if got := row["context_length"]; got != float64(500000) {
		t.Fatalf("declared combo context_length = %v, want 500000", got)
	}
	if got := row["max_output_tokens"]; got != float64(128000) {
		t.Fatalf("declared combo max_output_tokens = %v, want 128000", got)
	}

	// A combo WITHOUT a declaration still derives the leg minimum, and
	// a chain with an unknown leg still carries no limit at all.
	row, ok = rows["partial"]
	if !ok {
		t.Fatal("partial not advertised")
	}
	if _, has := row["context_length"]; has {
		t.Fatalf("underived combo with an unknown leg carries a context_length: %+v", row)
	}
}

// TestModelsAdvertisedIDSetUnchanged: the limits must not add, rename or
// drop a single advertised id.
func TestModelsAdvertisedIDSetUnchanged(t *testing.T) {
	cfg := limitsCfg()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	srv.freeIdx.Upsert(free.Entry{Provider: "opencode-go", Model: "minimax-m3.1-flash-free", Source: free.SourceCatalog, Context: 1000000, MaxOut: 512000})

	rows := modelsJSON(t, srv)
	var got []string
	for id := range rows {
		got = append(got, id)
	}
	sort.Strings(got)
	want := []string{
		"execution",
		"fast",
		"opencode-free/deepseek-v4.1-flash-free",
		"opencode/minimax-m3.1-flash-free",
		"opencode/unknown-model",
		"partial",
	}
	if len(got) != len(want) {
		t.Fatalf("advertised ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("advertised ids = %v, want %v", got, want)
		}
	}
}

// TestModelsDevProviderMapping: the kind mapping wins over the name
// match (the onegw provider named "opencode" is the subscription lane).
func TestModelsDevProviderMapping(t *testing.T) {
	tests := []struct {
		kind string
		name string
		want string
	}{
		{"opencode", "opencode", "opencode-go"},
		{"opencode-free", "opencode-free", "opencode"},
		{"openai", "xai", "xai"},
		{"openai", "opencode", "opencode"},
		{"", "anthropic", "anthropic"},
	}
	for _, tc := range tests {
		if got := modelsDevProvider(tc.kind, tc.name); got != tc.want {
			t.Fatalf("modelsDevProvider(%q, %q) = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
}
