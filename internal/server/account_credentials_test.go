package server

// Account-credential resolution. `api_key =` and ONEGW_PROVIDER_<NAME>_KEY both
// land on the PROVIDER's key, while the pool historically read that field only
// when the config declared no [[providers.accounts]] rows at all. A named row
// with an empty `api_key` — the shape a copied provider block keeps, and the
// shape a subscription account has before its first sign-in — therefore went out
// with a bare `Authorization: Bearer ` and every request took the vendor's 401
// (live 2026-09-16 with cline: same binary, same bearer, 200 with no account row,
// 401 with one). These pin both halves of the fix: the row inherits the provider
// key, and an OAuth-resolved token still outranks it.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"onegw/internal/config"
	"onegw/internal/oauth"
)

// newCredentialCfg builds a validated single-provider config and its server.
func newCredentialCfg(t *testing.T, p config.ProviderCfg, oauthAccts ...config.OAuthAccount) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "gw-key"}}
	cfg.Providers = []config.ProviderCfg{p}
	cfg.OAuth.Accounts = oauthAccts
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func completionStub(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
}

func gwChat(t *testing.T, s *Server, model string) {
	t.Helper()
	r := chatReq(t, model)
	r.Header.Set("Authorization", "Bearer gw-key")
	if w := do(t, s.Handler(), r); w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

// TestNamedAccountInheritsProviderKey is the regression: before the inheritance
// in apply(), the row's empty key won and the upstream saw no credential.
func TestNamedAccountInheritsProviderKey(t *testing.T) {
	var seen []string
	up := completionStub(t, &seen)
	defer up.Close()

	s := newCredentialCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up.URL, APIKey: "provider-level-key",
		Models:   []string{"m"},
		Accounts: []config.Acct{{Name: "a1"}}, // no api_key on the row
	})
	gwChat(t, s, "p1/m")

	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if seen[0] != "Bearer provider-level-key" {
		t.Errorf("upstream Authorization = %q, want the inherited provider key", seen[0])
	}
}

// TestOAuthTokenOutranksInheritedKey guards the precedence the inheritance
// depends on: filling a row's gap must never override the live credential the
// token store holds for a subscription account.
func TestOAuthTokenOutranksInheritedKey(t *testing.T) {
	var seen []string
	up := completionStub(t, &seen)
	defer up.Close()

	s := newCredentialCfg(t,
		config.ProviderCfg{
			Name: "sub", Kind: "openai", BaseURL: up.URL, APIKey: "stale-fallback-key",
			Models: []string{"m"}, Accounts: []config.Acct{{Name: "main"}},
		},
		config.OAuthAccount{Provider: "sub", Account: "main", Service: "kilocode"},
	)
	if err := s.oauth.Store().Put("sub/main", oauth.Token{
		AccessToken: "workos:live-session-token", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	gwChat(t, s, "sub/m")

	if len(seen) != 1 || seen[0] != "Bearer workos:live-session-token" {
		t.Errorf("upstream Authorization = %v, want the stored OAuth token to win", seen)
	}
}

// TestCredentiallessAccountStillLoads pins the deliberate softness: a row that
// can resolve no credential from anywhere must NOT fail validation or startup.
// A subscription account before its first sign-in is a legitimate state (the
// subquota poller skips it), and a config that fails to load bricks the next
// restart — the #99 failure mode. The operator is told at load instead.
func TestCredentiallessAccountStillLoads(t *testing.T) {
	s := newCredentialCfg(t, config.ProviderCfg{
		Name: "bare", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1",
		Models: []string{"m"}, Accounts: []config.Acct{{Name: "soon"}},
	})
	def, ok := s.cur().pool.Get("bare")
	if !ok {
		t.Fatal("provider missing from the pool")
	}
	if len(def.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want the one configured row", def.Accounts)
	}
	if def.Accounts[0].Name != "soon" || def.Accounts[0].APIKey != "" {
		t.Errorf("accounts = %+v, want the credential-less row kept addressable", def.Accounts)
	}
}
