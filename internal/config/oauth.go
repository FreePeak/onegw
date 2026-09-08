package config

import "fmt"

// OAuthAccount is one OAuth device-flow account: provider (xai, kilocode,
// ...) + account name. Tokens live in the data dir token store (managed by
// internal/oauth), never in TOML; at request time they are injected as the
// upstream bearer credential. The account must reference an existing
// [[providers]] entry — its Kind decides the wire format; the OAuth token
// replaces the static api_key.
// OAuthCfg groups the device-flow accounts: [[oauth.accounts]].
type OAuthCfg struct {
	Accounts []OAuthAccount `toml:"accounts"`
}

type OAuthAccount struct {
	Provider string `toml:"provider"` // provider config name ("xai")
	Account  string `toml:"account"`  // account name; default "default"
	// Service selects the OAuth provider profile when it differs from the
	// provider config name (e.g. provider "grok" using service "xai").
	// Default: same as Provider.
	Service string `toml:"service"`
	// Optional overrides of the service profile's endpoints (self-hosted
	// IdPs, tests). Empty = use the built-in service endpoints.
	DeviceURL string `toml:"device_url"`
	TokenURL  string `toml:"token_url"`
	ClientID  string `toml:"client_id"`
	Scope     string `toml:"scope"`
}

// OAuthAccounts returns normalized account entries: fills Account and
// Service defaults.
func (c *Config) OAuthAccounts() []OAuthAccount {
	out := make([]OAuthAccount, 0, len(c.OAuth.Accounts))
	for _, a := range c.OAuth.Accounts {
		if a.Account == "" {
			a.Account = "default"
		}
		if a.Service == "" {
			a.Service = a.Provider
		}
		out = append(out, a)
	}
	return out
}

// validateOAuth checks the [[oauth.accounts]] section: provider references
// must resolve, service profiles must be known, and no duplicate accounts.
func validateOAuth(c *Config) error {
	provNames := map[string]bool{}
	for _, p := range c.Providers {
		provNames[p.Name] = true
	}
	seen := map[string]bool{}
	for _, a := range c.OAuth.Accounts {
		if a.Provider == "" {
			return fmt.Errorf("oauth account missing provider")
		}
		if !provNames[a.Provider] {
			return fmt.Errorf("oauth account references unknown provider %q", a.Provider)
		}
		if a.Service != "" && !KnownOAuthService(a.Service) {
			return fmt.Errorf("oauth account %s/%s: unknown oauth service %q", a.Provider, a.Account, a.Service)
		}
		key := a.Provider + "/" + a.Account
		if seen[key] {
			return fmt.Errorf("duplicate oauth account %s", key)
		}
		seen[key] = true
	}
	return nil
}

// KnownOAuthService reports whether name is a built-in OAuth device-flow
// service profile.
func KnownOAuthService(name string) bool {
	switch name {
	case "xai", "kilocode":
		return true
	default:
		return false
	}
}
