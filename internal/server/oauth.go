package server

import (
	"log"
	"strings"
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
// token is not yet stored (not logged in).
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
			def.Accounts[i].SetTokenResolver(s.oauth.Token, a.Provider+"/"+a.Account)
		}
	}
}

// syncOAuth starts, restarts, and stops per-account refresh goroutines to
// match the config's [[oauth.accounts]] section. Called at the end of
// every apply (initial and reload).
func (s *Server) syncOAuth(cfg *config.Config) {
	if s.oauth == nil {
		return
	}
	specs := make([]oauth.AccountSpec, 0, len(cfg.OAuth.Accounts))
	for _, a := range cfg.OAuthAccounts() {
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
// means the account's credential is stale, so the pool skips it for
// oauthRefreshCool while the loop retries.
func (s *Server) coolOAuthAccount(key string) {
	prov, acct, ok := strings.Cut(key, "/")
	if !ok {
		return
	}
	st := s.cur()
	if st == nil {
		return
	}
	def, ok := st.pool.Get(prov)
	if !ok {
		return
	}
	for i := range def.Accounts {
		if def.Accounts[i].Name == acct {
			def.Cool(&def.Accounts[i], oauthRefreshCool)
			return
		}
	}
}
