package provider

import (
	"net/http"
	"strings"
	"time"
)

// time. OAuth device-flow accounts (#2) set it; static api_key accounts
// leave it nil and Do uses acct.APIKey. Keeping it an interface value on
// the Account avoids importing internal/oauth here (no cycle) and lets the
// refresher swap tokens without rebuilding pools.
type TokenProvider interface {
	// Token returns the current access token; "" = none available.
	Token() string
}

// managerToken adapts an oauth.Manager token lookup into TokenProvider.
// The manager is the gateway-wide token resolver (request-time read).
type managerToken struct {
	resolver func(key string) string
	key      string
}

func (m managerToken) Token() string {
	if m.resolver == nil {
		return ""
	}
	return m.resolver(m.key)
}

// SetTokenResolver attaches a live-token resolver to an account. resolver
// receives the account's oauth store key ("provider/account") and returns
// the current access token. This is the hook internal/oauth uses.
func (a *Account) SetTokenResolver(resolver func(key string) string, key string) {
	a.OAuthToken = managerToken{resolver: resolver, key: key}
}

// SetOAuthToken attaches an arbitrary TokenProvider to an account (tests,
// alternative token sources).
func (a *Account) SetOAuthToken(tp TokenProvider) { a.OAuthToken = tp }

// ClearOAuthToken detaches the token provider.
func (a *Account) ClearOAuthToken() { a.OAuthToken = nil }

// HasOAuthToken reports whether the account resolves its credential
// dynamically (OAuth) rather than the static APIKey.
func (a *Account) HasOAuthToken() bool { return a.OAuthToken != nil }

// bearerToken returns the credential for the account: the OAuth token when
// one resolves, else the static API key.
func (a *Account) bearerToken() string {
	if a.OAuthToken != nil {
		if t := a.OAuthToken.Token(); t != "" {
			return t
		}
	}
	return a.APIKey
}

// applyAuth sets the upstream credential headers for kind k.
func applyAuth(h http.Header, k Kind, tok string) {
	switch k {
	case KindAnthropic:
		h.Set("x-api-key", tok)
		h.Set("anthropic-version", "2023-06-01")
	case KindGemini:
		h.Set("x-goog-api-key", tok)
	default:
		h.Set("Authorization", "Bearer "+tok)
	}
}

// headerName returns the credential header for kind k (tests, diagnostics).
func headerName(k Kind) string {
	switch k {
	case KindAnthropic:
		return "x-api-key"
	case KindGemini:
		return "x-goog-api-key"
	default:
		return "Authorization"
	}
}

// oauthKey builds the token-store key for a provider/account pair:
// "<provider>/<account>".
func oauthKey(provider, account string) string {
	return provider + "/" + strings.TrimSpace(account)
}

// AccountState is a point-in-time snapshot of one rotation slot
// (diagnostics, tests).
type AccountState struct {
	Name    string
	Cooling bool
}

// PoolStates snapshots every account's cooldown state.
func (d *Def) PoolStates() []AccountState { return d.pool.states() }

func (p *accountPool) states() []AccountState {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]AccountState, 0, len(p.accts))
	for i := range p.accts {
		out = append(out, AccountState{
			Name:    p.accts[i].acct.Name,
			Cooling: now.Before(p.accts[i].cooldown),
		})
	}
	return out
}
