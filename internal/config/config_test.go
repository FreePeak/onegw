package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsNonHTTPExportURL(t *testing.T) {
	for _, url := range []string{"ftp://agg:8080/x", "agg:8080/admin/usage/import", "file:///tmp/x"} {
		c := &Config{Usage: UsageCfg{ExportURL: url}}
		if err := c.Validate(); err == nil {
			t.Fatalf("export_url %q must be rejected", url)
		}
	}
	c := &Config{Usage: UsageCfg{ExportURL: "https://agg:8080/admin/usage/import"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("https export_url must pass: %v", err)
	}
}

func TestValidateSkipsKeylessProviderOnLoopback(t *testing.T) {
	c := &Config{}
	c.Defaults() // loopback default bind
	c.Providers = []ProviderCfg{
		{Name: "good", Kind: "openai", APIKey: "sk-x", Models: []string{"m1"}},
		{Name: "keyless", Kind: "anthropic", BaseURL: "https://api.anthropic.com", Models: []string{"m2"}},
	}
	c.Combos = []ComboCfg{{Name: "mix", Targets: []string{"keyless/m2", "good/m1"}}}
	c.Aliases = map[string]string{"cheap": "keyless/m2"}
	if err := c.Validate(); err != nil {
		t.Fatalf("loopback boot must warn-and-skip, not fail: %v", err)
	}
	if len(c.Providers) != 1 || c.Providers[0].Name != "good" {
		t.Fatalf("keyless provider must be dropped from the active set, got %+v", c.Providers)
	}
	if len(c.Skipped) != 1 || c.Skipped[0] != "keyless" {
		t.Fatalf("Skipped must record the dropped provider, got %v", c.Skipped)
	}
}

func TestValidateFailsKeylessProviderOffLoopback(t *testing.T) {
	for _, listen := range []string{":8080", "0.0.0.0:8080", "192.168.1.10:8080", "example.com:443", ""} {
		c := &Config{}
		c.Server.Listen = listen
		c.Providers = []ProviderCfg{{Name: "keyless", Kind: "anthropic", BaseURL: "https://api.anthropic.com"}}
		err := c.Validate()
		if err == nil {
			t.Fatalf("listen %q: keyless provider must keep failing validation", listen)
		}
		if !strings.Contains(err.Error(), "needs api_key") {
			t.Fatalf("listen %q: wrong error: %v", listen, err)
		}
	}
}

func TestValidateLoopbackListenForms(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if !isLoopbackListen(listen) {
			t.Fatalf("%q must count as loopback", listen)
		}
	}
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "192.168.1.10:8080", "example.com:443"} {
		if isLoopbackListen(listen) {
			t.Fatalf("%q must NOT count as loopback", listen)
		}
	}
}

func TestValidateOAuthBackedProviderNotKeyless(t *testing.T) {
	c := &Config{}
	c.Defaults()
	c.Providers = []ProviderCfg{{Name: "xai", Kind: "openai", BaseURL: "https://api.x.ai"}}
	c.OAuth = OAuthCfg{Accounts: []OAuthAccount{{Provider: "xai"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("oauth-backed provider must not be treated as keyless: %v", err)
	}
	if len(c.Providers) != 1 || len(c.Skipped) != 0 {
		t.Fatalf("oauth-backed provider must stay active, got %+v skipped=%v", c.Providers, c.Skipped)
	}
}
