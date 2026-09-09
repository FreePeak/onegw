package config

import (
	"os"
	"strings"
	"testing"
)

// [server] task_routing (issue #54): "off" is the default and anything
// except "on" (case-insensitive) stays off; ONEGW_TASK_ROUTING overrides
// the file value, mirroring the other env overrides.
func TestTaskRoutingOn(t *testing.T) {
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
		cfg.Server.TaskRouting = c.val
		if got := cfg.TaskRoutingOn(); got != c.want {
			t.Errorf("task_routing %q: got %v, want %v", c.val, got, c.want)
		}
	}
}

func TestTaskRoutingEnvOverride(t *testing.T) {
	t.Setenv("ONEGW_TASK_ROUTING", "on")
	cfg := &Config{}
	cfg.Defaults()
	if !cfg.TaskRoutingOn() {
		t.Fatalf("ONEGW_TASK_ROUTING=on must enable task routing")
	}
}

// Validation: unknown values must fail the load; [[tier]] rows need a
// model and a 0–150 power; negative limits are rejected.
func TestValidateTaskRoutingAndTiers(t *testing.T) {
	base := func() *Config {
		cfg := &Config{}
		cfg.Providers = []ProviderCfg{{
			Name: "p", Kind: "openai", APIKey: "k",
			Tiers: []TierCfg{{Model: "m", Power: 100}},
		}}
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("baseline tier config rejected: %v", err)
	}

	bad := base()
	bad.Server.TaskRouting = "maybe"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "task_routing") {
		t.Fatalf("task_routing \"maybe\" must fail: %v", err)
	}

	badTier := base()
	badTier.Providers[0].Tiers[0].Power = 200
	if err := badTier.Validate(); err == nil || !strings.Contains(err.Error(), "power") {
		t.Fatalf("tier power 200 must fail: %v", err)
	}

	noModel := base()
	noModel.Providers[0].Tiers[0].Model = ""
	if err := noModel.Validate(); err == nil || !strings.Contains(err.Error(), "missing model") {
		t.Fatalf("tier without model must fail: %v", err)
	}

	negCtx := base()
	negCtx.Providers[0].Tiers[0].Context = -1
	if err := negCtx.Validate(); err == nil || !strings.Contains(err.Error(), "context/max_out") {
		t.Fatalf("negative context must fail: %v", err)
	}
}

// The full TOML path: [[providers.tier]] tables and [server] task_routing
// must parse into the config structs.
func TestLoadTaskRoutingTOML(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "onegw*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(`
[server]
task_routing = "on"
[[providers]]
name = "p"
kind = "openai"
api_key = "k"
[[providers.tier]]
model = "glm-5.3"
power = 100
vision = true
reasoning = true
context = 200000
max_out = 64000
`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TaskRoutingOn() {
		t.Fatalf("task_routing = on not loaded: %q", cfg.Server.TaskRouting)
	}
	tier := cfg.Providers[0].Tiers[0]
	if tier.Model != "glm-5.3" || tier.Power != 100 || !tier.Vision || !tier.Reasoning || tier.Context != 200000 || tier.MaxOut != 64000 {
		t.Fatalf("tier not loaded: %+v", tier)
	}
}
