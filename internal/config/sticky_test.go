package config

import "testing"

func TestValidateStickyDuration(t *testing.T) {
	ok := &Config{
		Auth:      Auth{KeyList: []AuthKey{{Key: "sk-1"}}},
		Providers: []ProviderCfg{{Name: "p", Kind: "openai", APIKey: "k", Sticky: "5m", Models: []string{"m"}}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf(`sticky "5m" must validate: %v`, err)
	}
	for _, bad := range []string{"garbage", "0s", "-5m", "5", "1h30"} {
		c := &Config{
			Auth:      Auth{KeyList: []AuthKey{{Key: "sk-1"}}},
			Providers: []ProviderCfg{{Name: "p", Kind: "openai", APIKey: "k", Sticky: bad, Models: []string{"m"}}},
		}
		if err := c.Validate(); err == nil {
			t.Fatalf("sticky %q must be rejected", bad)
		}
	}
}
