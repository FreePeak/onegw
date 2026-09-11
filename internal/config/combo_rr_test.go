package config

import (
	"strings"
	"testing"
)

// #82 config surface: the round-robin strategy is first-class, its limit is
// bounded, and a limit without the strategy is a typo the loader catches.
func TestRoundRobinComboConfig(t *testing.T) {
	base := func(strategy string, limit int) *Config {
		c := &Config{}
		c.Auth.KeyList = []AuthKey{{Key: "k"}}
		c.Providers = []ProviderCfg{{Name: "p1", Kind: "openai", APIKey: "k", Models: []string{"m1"}},
			{Name: "p2", Kind: "openai", APIKey: "k", Models: []string{"m2"}}}
		c.Combos = []ComboCfg{{Name: "rr", Targets: []string{"p1/m1", "p2/m2"}, Strategy: strategy, RoundRobinLimit: limit}}
		c.Defaults()
		return c
	}
	if err := base("round-robin", 2).Validate(); err != nil {
		t.Fatalf("round-robin with a limit must be valid: %v", err)
	}
	if err := base("round-robin", 0).Validate(); err != nil {
		t.Fatalf("round-robin without a limit must default: %v", err)
	}
	if err := base("order", 2).Validate(); err == nil || !strings.Contains(err.Error(), "without strategy") {
		t.Fatalf("a limit without the strategy must be rejected, got %v", err)
	}
	if err := base("round-robin", 1001).Validate(); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("an out-of-range limit must be rejected, got %v", err)
	}
	if err := base("roundrobin", 0).Validate(); err == nil || !strings.Contains(err.Error(), "must be") {
		t.Fatalf("an unknown strategy must be rejected, got %v", err)
	}
}
