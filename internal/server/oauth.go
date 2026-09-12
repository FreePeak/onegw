package server

import (
	"log"
	"time"

	"onegw/internal/config"
	"onegw/internal/oauth"
	"onegw/internal/provider"
)

// oauthRefreshCool is how long an account stays out of rotation after a
// failed token refresh (the stored credential is stale or revoked; the
// refresh loop retries on its next tick).
const oauthRefreshCool = 60 * time.Second

// initOAuth creates the process-wide OAuth manager. Like the usage store,
// the token store lives under the data dir fixed at startup, so the
// manager survives hot reloads; only its account set changes (via Sync).
func (s *Server) initOAuth(cfg *config.Config) {
	s.oauth = oauth.NewManager(oauth.NewTokenStore(cfg.Server.DataDir))
	s.oauth.Cooler = s.coolOAuthAccount
}

// wireOAuthTokens attaches live token resolvers to the accounts named by
// [[oauth.accounts]] (issue #2). It must run before pool.Set: the account
// pool copies Account values, so the resolver has to be in place first.
// The provider's static api_key stays as a fallback for accounts whose
// token is not yet stored (not logged in). An entry with `owner` set
// resolves the borrowed account's stored session — one SuperGrok login
// then carries every provider surface that shares it.
func (s *Server) wireOAuthTokens(cfg *config.Config, def *provider.Def) {
	if s.oauth == nil || len(cfg.OAuth.Accounts) == 0 {
		return
	}
	for _, a := range cfg.OAuthAccounts() {
		if a.Provider != def.Name {
			continue
		}
		for i := range def.Accounts {
			if def.Accounts[i].Name != a.Account {
				continue
			}
			def.Accounts[i].SetTokenResolver(s.oauth.Token, a.StoreKey())
		}
	}
}

// syncOAuth starts, restarts, and stops per-account refresh goroutines to
// match the config's [[oauth.accounts]] section. Called at the end of
// every apply (initial and reload). Borrowers (`owner` set) are skipped:
// rotating a device session twice per cycle would invalidate the copy the
// owner just stored, so exactly one entry owns each login.
func (s *Server) syncOAuth(cfg *config.Config) {
	if s.oauth == nil {
		return
	}
	specs := make([]oauth.AccountSpec, 0, len(cfg.OAuth.Accounts))
	for _, a := range cfg.OAuthAccounts() {
		if a.Owner != "" {
			continue // borrower: the owner entry refreshes this session
		}
		p, ok := oauth.Lookup(a.Service)
		if !ok {
			// config.Validate rejects unknown services; defensive so a
			// bad reload can never take the gateway down.
			log.Printf("oauth: unknown service %q for %s/%s, skipped", a.Service, a.Provider, a.Account)
			continue
		}
		// Per-account endpoint overrides (self-hosted IdPs, tests).
		if a.DeviceURL != "" {
			p.DeviceCodeURL = a.DeviceURL
		}
		if a.TokenURL != "" {
			p.TokenURL = a.TokenURL
		}
		if a.ClientID != "" {
			p.ClientID = a.ClientID
		}
		if a.Scope != "" {
			p.Scope = a.Scope
		}
		specs = append(specs, oauth.AccountSpec{
			Key:      a.Provider + "/" + a.Account,
			Provider: p,
		})
	}
	s.oauth.Sync(specs)
}

// coolOAuthAccount is the manager's failure hook: a refresh that failed
// means the credential behind the store key is stale, so the pool skips
// every account that resolves that key (the owner plus any `owner`
// borrower) for oauthRefreshCool while the loop retries.
func (s *Server) coolOAuthAccount(key string) {
	st := s.cur()
	if st == nil {
		return
	}
	for _, a := range st.cfg.OAuthAccounts() {
		if a.StoreKey() != key {
			continue
		}
		def, ok := st.pool.Get(a.Provider)
		if !ok {
			continue
		}
		for i := range def.Accounts {
			if def.Accounts[i].Name == a.Account {
				def.Cool(&def.Accounts[i], oauthRefreshCool)
			}
		}
	}
}
