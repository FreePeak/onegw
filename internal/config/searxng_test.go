package config

import (
	"strings"
	"testing"
)

func searxProviders() []ProviderCfg {
	return []ProviderCfg{
		{Name: "search", Kind: "searxng", BaseURL: "http://searx:8080"},
		{Name: "real", Kind: "openai", APIKey: "sk-test-up"},
	}
}

func TestValidateSearxngNeedsNoCredentials(t *testing.T) {
	c := &Config{Providers: searxProviders()}
	if err := c.Validate(); err != nil {
		t.Fatalf("public SearXNG instance without api_key must validate: %v", err)
	}
}

func TestValidateSearxngRequiresBaseURL(t *testing.T) {
	c := &Config{Providers: []ProviderCfg{{Name: "search", Kind: "searxng"}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("searxng without base_url: got %v, want base_url error", err)
	}
}

func TestValidateAcceptsCustomWireKinds(t *testing.T) {
	// issue #12 kinds: valid in config; openai-responses serves the
	// OpenAI Responses API (orcarouter, Grok CLI proxy), commandcode is
	// NDJSON, cursor fails fast at request time.
	for _, kind := range []string{"openai-responses", "commandcode", "cursor"} {
		c := &Config{Providers: []ProviderCfg{{Name: "x", Kind: kind, APIKey: "k"}}}
		if err := c.Validate(); err != nil {
			t.Fatalf("kind %q must validate: %v", kind, err)
		}
	}
}

func TestValidateRejectsUnknownKind(t *testing.T) {
	c := &Config{Providers: []ProviderCfg{{Name: "x", Kind: "nope", APIKey: "k"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestValidateStillRequiresKeysForChatKinds(t *testing.T) {
	c := &Config{Providers: []ProviderCfg{{Name: "real", Kind: "openai"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("openai provider without key/accounts must still be rejected")
	}
}
