package config

import (
	"os"
	"strings"
	"testing"
)

// [server] combo_fallback: "off" is the default and anything except "on"
// (case-insensitive) stays off; ONEGW_COMBO_FALLBACK overrides the file.
func TestComboFallbackOn(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false}, // default
		{"off", false},
		{"OFF", false},
		{"on", true},
		{"ON", true},
		{" On ", true},
		{"bogus", false},
	}
	for _, c := range cases {
		cfg := &Config{}
		cfg.Server.ComboFallback = c.val
		if got := cfg.ComboFallbackOn(); got != c.want {
			t.Errorf("combo_fallback %q: got %v, want %v", c.val, got, c.want)
		}
	}
}

func TestComboFallbackEnvOverride(t *testing.T) {
	t.Setenv("ONEGW_COMBO_FALLBACK", "on")
	cfg := &Config{}
	cfg.Defaults()
	if !cfg.ComboFallbackOn() {
		t.Fatalf("ONEGW_COMBO_FALLBACK=on must enable combo fallback")
	}
}

func TestValidateComboFallback(t *testing.T) {
	cfg := &Config{}
	cfg.Providers = []ProviderCfg{{Name: "p", Kind: "openai", APIKey: "k"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	bad := *cfg
	bad.Server.ComboFallback = "maybe"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "combo_fallback") {
		t.Fatalf("combo_fallback %q must fail: %v", bad.Server.ComboFallback, err)
	}
}

func TestLoadComboFallbackTOML(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "onegw*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(`
[server]
combo_fallback = "on"
[[providers]]
name = "p"
kind = "openai"
api_key = "k"
`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ComboFallbackOn() {
		t.Fatalf("combo_fallback = on not loaded: %q", cfg.Server.ComboFallback)
	}
}
