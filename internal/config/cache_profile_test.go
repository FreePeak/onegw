package config

import (
	"strings"
	"testing"
)

// cache_profile is an opt-in enum: unknown values must fail the load
// rather than silently forwarding mutated bodies (issue #34).
func TestValidateCacheProfile(t *testing.T) {
	for _, ok := range []string{"", "none", "claude-anchor", "dashscope-marker", "sticky-key"} {
		c := &Config{
			Auth:      Auth{KeyList: []AuthKey{{Key: "sk-1"}}},
			Providers: []ProviderCfg{{Name: "p", Kind: "openai", APIKey: "k", Models: []string{"m"}, CacheProfile: ok}},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("cache_profile %q must validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"claude", "anthropic", "auto", "NONE", "sticky"} {
		c := &Config{
			Auth:      Auth{KeyList: []AuthKey{{Key: "sk-1"}}},
			Providers: []ProviderCfg{{Name: "p", Kind: "openai", APIKey: "k", Models: []string{"m"}, CacheProfile: bad}},
		}
		err := c.Validate()
		if err == nil {
			t.Fatalf("cache_profile %q must be rejected", bad)
		}
		if !strings.Contains(err.Error(), "cache_profile") {
			t.Fatalf("rejection must name the knob: %v", err)
		}
	}
}
