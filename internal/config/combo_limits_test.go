package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A combo may declare its own context window and output cap: the
// operator's word on what the chain as a whole carries. 0 means "derive
// it from the legs"; a negative value is a typo the loader catches.
func TestComboDeclaredLimits(t *testing.T) {
	base := func(context, maxOut int) *Config {
		c := &Config{}
		c.Auth.KeyList = []AuthKey{{Key: "k"}}
		c.Providers = []ProviderCfg{{Name: "p1", Kind: "openai", APIKey: "k", Models: []string{"m1"}}}
		c.Combos = []ComboCfg{{Name: "exec", Targets: []string{"p1/m1"}, Context: context, MaxOut: maxOut}}
		c.Defaults()
		return c
	}
	if err := base(500000, 128000).Validate(); err != nil {
		t.Fatalf("a declared context must be valid: %v", err)
	}
	if err := base(0, 0).Validate(); err != nil {
		t.Fatalf("no declaration must be valid (derive from the legs): %v", err)
	}
	if err := base(-1, 0).Validate(); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("a negative context must be rejected, got %v", err)
	}
	if err := base(0, -1).Validate(); err == nil || !strings.Contains(err.Error(), "max_out") {
		t.Fatalf("a negative max_out must be rejected, got %v", err)
	}
}

// The declaration survives a TOML round trip: a combo block that spells
// out context and max_out loads with both values set.
func TestComboDeclaredLimitsFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	body := `
[[providers]]
name = "p1"
kind = "openai"
api_key = "k"
models = ["m1"]

[[combo]]
name = "exec"
targets = ["p1/m1"]
context = 500000
max_out = 128000
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Combos) != 1 {
		t.Fatalf("combos = %d, want 1", len(cfg.Combos))
	}
	if cfg.Combos[0].Context != 500000 {
		t.Fatalf("combo context = %d, want 500000", cfg.Combos[0].Context)
	}
	if cfg.Combos[0].MaxOut != 128000 {
		t.Fatalf("combo max_out = %d, want 128000", cfg.Combos[0].MaxOut)
	}
}
