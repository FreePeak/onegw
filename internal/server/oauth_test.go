package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"onegw/internal/config"
	"onegw/internal/oauth"
)

// fakeIdP is a hermetic RFC 8628 device authorization server + upstream
// bearer probe: the gateway's oauth account logs in against it, refreshes
// against it, and the "upstream" echoes the Authorization header it saw.
type fakeIdP struct {
	mu         sync.Mutex
	refreshes  int
	starts     int
	deny       bool // when true, token polls answer access_denied
	seenBearer atomic.Value
}

func (f *fakeIdP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.starts++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "DEV-1",
			"user_code":                 "CODE-1",
			"verification_uri":          "https://idp.example/activate",
			"verification_uri_complete": "https://idp.example/activate?code=CODE-1",
			"expires_in":                30,
			"interval":                  1,
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if f.deny {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-live-1",
				"refresh_token": "rt-live-1",
				"expires_in":    3600,
			})
		case "refresh_token":
			f.refreshes++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-live-2",
				"refresh_token": "rt-live-2",
				"expires_in":    3600,
			})
		default:
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
		}
	})
	return mux
}

// start builds the IdP plus an upstream that records the Authorization
// header (the token injection point under test).
func (f *fakeIdP) start(t *testing.T) (idpURL, upstreamURL string) {
	t.Helper()
	idp := httptest.NewServer(f.handler())
	t.Cleanup(idp.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seenBearer.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	return idp.URL, up.URL
}

// oauthCfg builds a config with provider "xai" backed by upstream and an
// [[oauth.accounts]] entry pointing at the fake IdP.
func oauthCfg(t *testing.T, idpURL, upstreamURL string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Server.AdminPassword = "pw"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{{
		Name:    "xai",
		Kind:    "openai",
		BaseURL: upstreamURL,
		Accounts: []config.Acct{{
			Name: "main",
			// Deliberately wrong static key: proves the OAuth token wins.
			APIKey: "sk-test-static-fallback",
		}},
	}}
	cfg.OAuth.Accounts = []config.OAuthAccount{{
		Provider:  "xai",
		Account:   "main",
		Service:   "xai",
		DeviceURL: idpURL + "/device",
		TokenURL:  idpURL + "/token",
		ClientID:  "test-client",
		Scope:     "api:access",
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("oauth config invalid: %v", err)
	}
	return cfg
}

// loginOAuth performs the device flow through the server's own manager so
// the stored token is exactly what the gateway will inject. The spec is
// resolved from cfg exactly like the CLI does.
func loginOAuth(t *testing.T, s *Server, cfg *config.Config) {
	t.Helper()
	if s.oauth == nil {
		t.Fatal("server has no oauth manager")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := cfg.OAuthAccounts()[0]
	p, _ := oauth.Lookup(a.Service)
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
	tok, err := s.oauth.Login(ctx, oauth.AccountSpec{Key: a.Provider + "/" + a.Account, Provider: p}, nil)
	if err != nil {
		t.Fatalf("oauth login: %v", err)
	}
	if tok.AccessToken != "at-live-1" {
		t.Fatalf("unexpected token %q", tok.AccessToken)
	}
}

func oauthChat(model, gwKey string) *http.Request {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+gwKey)
	return req
}

func TestOAuthTokenInjectedUpstream(t *testing.T) {
	f := &fakeIdP{}
	idpURL, upstreamURL := f.start(t)
	cfg := oauthCfg(t, idpURL, upstreamURL)

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer s.Close()

	// Before login: the static fallback key is used (account still serves).
	rec := do(t, s.Handler(), oauthChat("xai/grok-4.6", "sk-test-gw"))
	if rec.Code != 200 {
		t.Fatalf("pre-login request: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.seenBearer.Load().(string); got != "Bearer sk-test-static-fallback" {
		t.Fatalf("pre-login bearer = %q", got)
	}

	// Login via device flow: the account now carries a live token.
	loginOAuth(t, s, cfg)

	rec = do(t, s.Handler(), oauthChat("xai/grok-4.6", "sk-test-gw"))
	if rec.Code != 200 {
		t.Fatalf("post-login request: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.seenBearer.Load().(string); got != "Bearer at-live-1" {
		t.Fatalf("upstream saw %q, want OAuth access token", got)
	}
}

func TestOAuthOwnerBorrowSharesOneSession(t *testing.T) {
	// One SuperGrok device login carries every surface that borrows it:
	// xAI rotates device sessions, so a second holder would knock the
	// first out. The borrower resolves the owner's token and stores none.
	f := &fakeIdP{}
	idpURL, upstreamURL := f.start(t)
	cfg := oauthCfg(t, idpURL, upstreamURL)
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name:     "grokbuild",
		Kind:     "openai",
		BaseURL:  upstreamURL,
		Accounts: []config.Acct{{Name: "main", APIKey: "sk-test-static-fallback"}},
	})
	cfg.OAuth.Accounts = append(cfg.OAuth.Accounts, config.OAuthAccount{
		Provider: "grokbuild", Account: "main", Service: "xai", Owner: "xai/main",
	})
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("borrow config invalid: %v", err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loginOAuth(t, s, cfg)

	for _, model := range []string{"xai/grok-4.6", "grokbuild/grok-build-0.1"} {
		rec := do(t, s.Handler(), oauthChat(model, "sk-test-gw"))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", model, rec.Code, rec.Body.String())
		}
		if got := f.seenBearer.Load().(string); got != "Bearer at-live-1" {
			t.Fatalf("%s upstream saw %q, want the owner's session token", model, got)
		}
	}
	if keys := s.oauth.Store().Keys(); len(keys) != 1 || keys[0] != "xai/main" {
		t.Fatalf("token store keys = %v, want only the owner session", keys)
	}
}

func TestOAuthRefreshBeforeExpirySwap(t *testing.T) {
	f := &fakeIdP{}
	idpURL, upstreamURL := f.start(t)
	cfg := oauthCfg(t, idpURL, upstreamURL)

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loginOAuth(t, s, cfg)

	// Shrink the stored expiry into the refresh lead and tighten the loop
	// so a refresh happens within the test.
	s.oauth.SetCheckEvery(10 * time.Millisecond)
	s.oauth.SetLead(time.Hour) // everything is "due"
	store := s.oauth.Store()
	tok, _ := store.Get("xai/main")
	tok.ExpiresAt = time.Now().Add(50 * time.Millisecond)
	if err := store.Put("xai/main", tok); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cur, _ := store.Get("xai/main"); cur.AccessToken == "at-live-2" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cur, _ := store.Get("xai/main")
	if cur.AccessToken != "at-live-2" {
		t.Fatalf("token never refreshed: %+v", cur)
	}

	// New requests now carry the rotated token.
	rec := do(t, s.Handler(), oauthChat("xai/grok-4.6", "sk-test-gw"))
	if rec.Code != 200 {
		t.Fatalf("post-refresh request: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.seenBearer.Load().(string); got != "Bearer at-live-2" {
		t.Fatalf("upstream saw %q after refresh", got)
	}
}

func TestOAuthReloadKeepsManagerAndSwapsSpecs(t *testing.T) {
	f := &fakeIdP{}
	idpURL, upstreamURL := f.start(t)
	cfg := oauthCfg(t, idpURL, upstreamURL)

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mgrBefore := s.oauth
	loginOAuth(t, s, cfg)

	// Reload with the same oauth section: the manager (and its stored
	// tokens) must survive; refresh loops keep running.
	s.Reload(cfg)
	if s.oauth != mgrBefore {
		t.Fatal("reload replaced the oauth manager")
	}
	if tok, _ := mgrBefore.Store().Get("xai/main"); tok.AccessToken != "at-live-1" {
		t.Fatal("reload lost stored token")
	}

	// Reload with the oauth section removed: loops stop, but the token
	// store (disk state) is untouched.
	cfgNoOAuth := oauthCfg(t, idpURL, upstreamURL)
	cfgNoOAuth.OAuth.Accounts = nil
	s.Reload(cfgNoOAuth)
	if len(mgrBefore.Specs()) != 0 {
		t.Fatalf("oauth loops still running after removal: %v", mgrBefore.Specs())
	}
	if _, ok := mgrBefore.Store().Get("xai/main"); !ok {
		t.Fatal("token store was wiped on reload")
	}

	// Requests fall back to the static key again.
	rec := do(t, s.Handler(), oauthChat("xai/grok-4.6", "sk-test-gw"))
	if rec.Code != 200 {
		t.Fatalf("post-removal request: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.seenBearer.Load().(string); got != "Bearer sk-test-static-fallback" {
		t.Fatalf("post-removal bearer = %q", got)
	}
}

func TestOAuthConfigValidation(t *testing.T) {
	t.Run("unknown provider rejected", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.DataDir = "memory"
		cfg.OAuth.Accounts = []config.OAuthAccount{{Provider: "ghost", Account: "a"}}
		cfg.Defaults()
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "unknown provider") {
			t.Fatalf("want unknown provider error, got %v", err)
		}
	})
	t.Run("unknown service rejected", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.DataDir = "memory"
		cfg.Providers = []config.ProviderCfg{{Name: "xai", Kind: "openai", APIKey: "sk-test-x"}}
		cfg.OAuth.Accounts = []config.OAuthAccount{{Provider: "xai", Service: "ghostservice"}}
		cfg.Defaults()
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "unknown oauth service") {
			t.Fatalf("want unknown service error, got %v", err)
		}
	})
	t.Run("duplicate account rejected", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.DataDir = "memory"
		cfg.Providers = []config.ProviderCfg{{Name: "xai", Kind: "openai", APIKey: "sk-test-x"}}
		cfg.OAuth.Accounts = []config.OAuthAccount{
			{Provider: "xai", Account: "main"},
			{Provider: "xai", Account: "main"},
		}
		cfg.Defaults()
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate oauth account") {
			t.Fatalf("want duplicate error, got %v", err)
		}
	})
	t.Run("defaults filled", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.DataDir = "memory"
		cfg.Providers = []config.ProviderCfg{{Name: "xai", Kind: "openai", APIKey: "sk-test-x"}}
		cfg.OAuth.Accounts = []config.OAuthAccount{{Provider: "xai"}}
		cfg.Defaults()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		accts := cfg.OAuthAccounts()
		if len(accts) != 1 || accts[0].Account != "default" || accts[0].Service != "xai" {
			t.Fatalf("defaults wrong: %+v", accts)
		}
	})

	t.Run("owner borrow validated", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.DataDir = "memory"
		cfg.Providers = []config.ProviderCfg{
			{Name: "xai", Kind: "openai", APIKey: "sk-test-x"},
			{Name: "grokbuild", Kind: "openai", APIKey: "sk-test-g"},
		}
		cfg.OAuth.Accounts = []config.OAuthAccount{
			{Provider: "xai", Account: "main", Service: "xai"},
			{Provider: "grokbuild", Account: "main", Service: "xai", Owner: "xai/main"},
		}
		cfg.Defaults()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		cfg.OAuth.Accounts[1].Owner = "grokbuild/main"
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "borrows itself") {
			t.Fatalf("want self-borrow error, got %v", err)
		}
		cfg.OAuth.Accounts[1].Owner = "ghost/none"
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not a declared") {
			t.Fatalf("want dangling-owner error, got %v", err)
		}
	})
}

func TestOAuthAccountCoolingOnFailedRefresh(t *testing.T) {
	// The refresh grant always 400s: refresh fails → the failure hook must
	// cool the pool slot for the account.
	var refreshFail atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "DEV-1", "user_code": "C",
			"verification_uri": "https://idp.example/a", "expires_in": 30, "interval": 1,
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-live-1", "refresh_token": "rt-live-1", "expires_in": 3600,
			})
		case "refresh_token":
			refreshFail.Add(1)
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		}
	})
	idp := httptest.NewServer(mux)
	defer idp.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	cfg := oauthCfg(t, idp.URL, up.URL)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loginOAuth(t, s, cfg)

	def, ok := s.cur().pool.Get("xai")
	if !ok {
		t.Fatal("provider missing")
	}
	// Force expiry inside the lead and let the loop fail.
	s.oauth.SetCheckEvery(10 * time.Millisecond)
	s.oauth.SetLead(time.Hour)
	store := s.oauth.Store()
	tok, _ := store.Get("xai/main")
	tok.ExpiresAt = time.Now().Add(30 * time.Millisecond)
	if err := store.Put("xai/main", tok); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && refreshFail.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if refreshFail.Load() == 0 {
		t.Fatal("refresh never attempted/failed")
	}
	// The failure hook must have cooled every slot of the account.
	deadline = time.Now().Add(2 * time.Second)
	for {
		cooling := false
		for _, st := range def.PoolStates() {
			if st.Name == "main" && st.Cooling {
				cooling = true
			}
		}
		if cooling {
			return // cooled: the router skips the account for oauthRefreshCool
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("account 'main' not cooling after failed refresh: %+v", def.PoolStates())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
