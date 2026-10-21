package free

import (
	"strings"
	"testing"
)

// TestParseModelList covers the three catalog shapes the upstreams emit,
// the junk shapes, and the oversize guard.
func TestParseModelList(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []Entry
	}{
		{
			name: "models.dev provider map",
			body: `{"opencode-go":{"id":"opencode-go","models":{"minimax-m3.1-flash-free":{"id":"minimax-m3.1-flash-free","limit":{"context":1000000,"output":512000}},"deepseek-v4.1-flash":{"limit":{"context":1000000,"output":384000}}}}}`,
			want: []Entry{
				{Provider: "opencode-go", Model: "deepseek-v4.1-flash", Free: false, Source: SourceCatalog, Context: 1000000, MaxOut: 384000},
				{Provider: "opencode-go", Model: "minimax-m3.1-flash-free", Free: true, Source: SourceCatalog, Context: 1000000, MaxOut: 512000},
			},
		},
		{
			name: "openrouter data list",
			body: `{"data":[{"id":"vendor/model-x","context_length":128000,"pricing":{"prompt":"0","completion":"0"}},{"id":"vendor/model-y"}]}`,
			want: []Entry{
				{Provider: "vendor", Model: "model-x", Free: false, Source: SourceCatalog, Context: 128000, MaxOut: 0},
				{Provider: "vendor", Model: "model-y", Free: false, Source: SourceCatalog, Context: 0, MaxOut: 0},
			},
		},
		{
			name: "plain data list",
			body: `{"data":[{"id":"plain-model"}]}`,
			want: []Entry{
				{Provider: "", Model: "plain-model", Free: false, Source: SourceCatalog, Context: 0, MaxOut: 0},
			},
		},
		{
			name: "plain models list",
			body: `{"models":[{"id":"other-model","context_window":64000,"max_output_tokens":8192}]}`,
			want: []Entry{
				{Provider: "", Model: "other-model", Free: false, Source: SourceCatalog, Context: 64000, MaxOut: 8192},
			},
		},
		{
			name: "models.dev single provider with one model",
			body: `{"opencode":{"models":{"deepseek-v4.1-flash-free":{"limit":{"context":200000,"output":32000}}}}}`,
			want: []Entry{
				{Provider: "opencode", Model: "deepseek-v4.1-flash-free", Free: true, Source: SourceCatalog, Context: 200000, MaxOut: 32000},
			},
		},
		{
			name: "junk object",
			body: `{"hello":"world"}`,
			want: nil,
		},
		{
			name: "junk array",
			body: `[1,2,3]`,
			want: nil,
		},
		{
			name: "not json",
			body: `<html>502</html>`,
			want: nil,
		},
		{
			name: "empty body",
			body: ``,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseModelList([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("rows = %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("row %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestParseModelListOversize refuses a body over the 8 MiB cap instead of
// reading it into memory.
func TestParseModelListOversize(t *testing.T) {
	body := []byte(`{"data":[{"id":"` + strings.Repeat("x", MaxCatalogBytes) + `"}]}`)
	if got := ParseModelList(body); got != nil {
		t.Fatalf("oversize body parsed into %d rows, want none", len(got))
	}
}

// TestIsFreeID covers the three free spellings.
func TestIsFreeID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"zai-org/GLM-4.6V:free", true},
		{"deepseek-v4.1-flash-free", true},
		{"vendor/free", true},
		{"grok-4.5", false},
		{"", false},
		{"freemium", false},
		{"vendor/free-model", false},
	}
	for _, tc := range tests {
		if got := IsFreeID(tc.id); got != tc.want {
			t.Fatalf("IsFreeID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestIndexUpsertAdditive covers the additive-only rule: a parsed row
// never lowers a known value, and a static declaration wins.
func TestIndexUpsertAdditive(t *testing.T) {
	tests := []struct {
		name    string
		upserts []Entry
		want    Entry
	}{
		{
			name:    "catalog row fills an unknown value",
			upserts: []Entry{{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000}},
			want:    Entry{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000},
		},
		{
			name: "catalog row never lowers a known value",
			upserts: []Entry{
				{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000, MaxOut: 200},
				{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 10, MaxOut: 20},
			},
			want: Entry{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000, MaxOut: 200},
		},
		{
			name: "a later row fills the missing half",
			upserts: []Entry{
				{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000},
				{Provider: "opencode-go", Model: "m", Source: SourceCatalog, MaxOut: 200},
			},
			want: Entry{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000, MaxOut: 200},
		},
		{
			name: "a static declaration wins over a catalog row",
			upserts: []Entry{
				{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000},
				{Provider: "opencode-go", Model: "m", Source: SourceConfig, Context: 4096, MaxOut: 512},
			},
			want: Entry{Provider: "opencode-go", Model: "m", Source: SourceConfig, Context: 4096, MaxOut: 512},
		},
		{
			name: "a catalog row never lowers a static declaration",
			upserts: []Entry{
				{Provider: "opencode-go", Model: "m", Source: SourceConfig, Context: 4096},
				{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 10},
			},
			want: Entry{Provider: "opencode-go", Model: "m", Source: SourceConfig, Context: 4096},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix := NewIndex()
			for _, e := range tc.upserts {
				ix.Upsert(e)
			}
			got, ok := ix.Lookup("opencode-go", "m")
			if !ok {
				t.Fatal("entry not found")
			}
			if got != tc.want {
				t.Fatalf("entry = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestIndexReplaceKeepsStaticDeclarations: a fresh catalog snapshot must
// not drop what the operator declared by hand.
func TestIndexReplaceKeepsStaticDeclarations(t *testing.T) {
	ix := NewIndex()
	ix.Upsert(Entry{Provider: "opencode-go", Model: "m", Source: SourceConfig, Context: 4096})
	ix.Replace([]Entry{{Provider: "opencode-go", Model: "m", Source: SourceCatalog, Context: 1000}})
	got, ok := ix.Lookup("opencode-go", "m")
	if !ok {
		t.Fatal("entry not found")
	}
	if got.Context != 4096 {
		t.Fatalf("context = %d, want the declared 4096", got.Context)
	}
}

// TestIndexLookupFallsBackToWildcard: a row that arrived without a
// provider is still found by a provider-scoped lookup.
func TestIndexLookupFallsBackToWildcard(t *testing.T) {
	ix := NewIndex()
	ix.Upsert(Entry{Provider: "", Model: "plain", Source: SourceCatalog, Context: 4096})
	got, ok := ix.Lookup("opencode-go", "plain")
	if !ok {
		t.Fatal("wildcard row not found")
	}
	if got.Context != 4096 {
		t.Fatalf("context = %d, want 4096", got.Context)
	}
}
