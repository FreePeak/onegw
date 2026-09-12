package config

import "testing"

// The opencode-free kind is the OpenCode Zen FREE tier: keyless by design,
// so the api_key/keys/accounts requirement that every other upstream kind
// enforces must not fire. Unknown kinds still fail loudly.
func TestValidateOpenCodeFreeKeyless(t *testing.T) {
	c := &Config{Providers: []ProviderCfg{{Name: "ocfree", Kind: "opencode-free"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("keyless opencode-free must validate, got %v", err)
	}
	// ...while the paid kind still demands a credential.
	paid := &Config{Providers: []ProviderCfg{{Name: "opencode", Kind: "opencode"}}}
	if err := paid.Validate(); err == nil {
		t.Fatal("credential-less opencode (Go) must still be rejected")
	}
	bad := &Config{Providers: []ProviderCfg{{Name: "x", Kind: "opencode-freee"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}
