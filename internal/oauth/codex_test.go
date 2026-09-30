package oauth

// The ChatGPT login surface, end to end against a stub authorization server:
// the authorize URL the operator opens carries the CLI's own parameters, the
// exchange posts the PKCE form, and the workspace id / plan decode off the
// access token the gateway will send upstream.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// codexTestJWT builds an unsigned ChatGPT-shaped access token: only the
// payload is ever decoded (CodexAccountID / CodexPlan), so a fake signature is
// enough and no signing key belongs in a test.
func codexTestJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestCodexProfileIsBrowserOnly(t *testing.T) {
	p, ok := Lookup("codex")
	if !ok {
		t.Fatal("codex profile is not registered")
	}
	if !p.BrowserFlow() {
		t.Error("codex must be a browser (PKCE) profile")
	}
	if p.DeviceCodeURL != "" {
		t.Errorf("codex has no device grant, got DeviceCodeURL=%q", p.DeviceCodeURL)
	}
	if p.ClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Errorf("client id = %q, want the Codex CLI's public client", p.ClientID)
	}
	// ChatGPT allow-lists the CLI's loopback redirect, so a wrong port or
	// path is an unrecoverable rejection at the vendor. The named constant
	// and the profile must agree, or the docs and the wire drift apart.
	if p.RedirectURI(0) != CodexRedirectURI {
		t.Errorf("redirect = %q, want the Codex CLI's registered loopback %q", p.RedirectURI(0), CodexRedirectURI)
	}
	if p.TokenURL != "https://auth.openai.com/oauth/token" {
		t.Errorf("token url = %q", p.TokenURL)
	}
	if !strings.Contains(p.Scope, "offline_access") {
		t.Errorf("scope %q lacks offline_access, so no refresh token is issued", p.Scope)
	}
}

func TestCodexAuthorizeURLMatchesCLI(t *testing.T) {
	p, _ := Lookup("codex")
	sess, err := NewPKCE(p, "")
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	u, err := url.Parse(sess.AuthURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := u.Query()
	if u.Path != "/oauth/authorize" || u.Host != "auth.openai.com" {
		t.Errorf("authorize endpoint = %s", u)
	}
	for k, want := range map[string]string{
		"response_type":         "code",
		"client_id":             p.ClientID,
		"code_challenge_method": "S256",
		"redirect_uri":          CodexRedirectURI,
		// The CLI sends these three; the vendor keys the simplified login
		// page on them.
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 CodexOriginator,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("authorize param %s = %q, want %q", k, got, want)
		}
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Error("authorize url is missing code_challenge or state")
	}
	if q.Get("scope") != p.Scope {
		t.Errorf("scope = %q, want %q", q.Get("scope"), p.Scope)
	}
}

func TestCodexExchangeCodeStoresToken(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}`))
	}))
	defer srv.Close()

	p, _ := Lookup("codex")
	p.TokenURL = srv.URL
	tok, err := p.ExchangeCode(t.Context(), nil, "the-code", CodexRedirectURI, "verifier-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if gotForm.Get("grant_type") != "authorization_code" ||
		gotForm.Get("code") != "the-code" ||
		gotForm.Get("code_verifier") != "verifier-1" ||
		gotForm.Get("client_id") != p.ClientID {
		t.Errorf("exchange form = %v", gotForm)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Errorf("token = %+v", tok)
	}
	if d := time.Until(tok.ExpiresAt); d <= 0 || d > 2*time.Hour {
		t.Errorf("ExpiresAt = %v (about %v away), want ~1h", tok.ExpiresAt, d)
	}
}

func TestCodexAccountIDAndPlanDecodeOffBearer(t *testing.T) {
	tok := codexTestJWT(t, map[string]any{
		"email": "someone@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "workspace-abc",
			"chatgpt_plan_type":  "plus",
		},
	})
	if got := CodexAccountID(tok); got != "workspace-abc" {
		t.Errorf("CodexAccountID = %q, want workspace-abc", got)
	}
	if got := CodexPlan(tok); got != "plus" {
		t.Errorf("CodexPlan = %q, want plus", got)
	}
}

// The negative half is the load-bearing one: a bearer that is NOT a ChatGPT
// credential must yield "", so no workspace id is ever invented and a plain
// api-key account is never mistaken for a ChatGPT one.
func TestCodexAccountIDRejectsNonChatGPTToken(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"opaque":      "sk-proj-abc123",
		"truncated":   "header.payload",
		"no claims":   codexTestJWT(t, map[string]any{"email": "a@b.c"}),
		"bad base64":  "eyJhbGciOiJub25lIn0.!!!not-base64!!!.sig",
		"wrong names": codexTestJWT(t, map[string]any{"chatgpt_account_id": "workspace-abc"}),
	}
	for name, tok := range cases {
		if got := CodexAccountID(tok); got != "" {
			t.Errorf("%s: CodexAccountID = %q, want empty", name, got)
		}
		if got := CodexPlan(tok); got != "" {
			t.Errorf("%s: CodexPlan = %q, want empty", name, got)
		}
	}
}

// A refresh must not carry `scope`: OpenAI's authorization server treats that
// as a re-scope and invalidates sibling refresh-token families on the same
// client id. Asserted here because the shared manager builds this form.
func TestCodexRefreshOmitsScope(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`))
	}))
	defer srv.Close()

	p, _ := Lookup("codex")
	p.TokenURL = srv.URL
	mgr := NewManager(NewTokenStore("memory"))
	defer mgr.Stop()
	spec := AccountSpec{Key: "codex/main", Provider: p}
	if err := mgr.store.Put(spec.Key, Token{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	mgr.Sync([]AccountSpec{spec})
	if _, err := mgr.refresh(context.Background(), spec); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "rt-1" {
		t.Errorf("refresh form = %v", gotForm)
	}
	if gotForm.Get("scope") != "" {
		t.Errorf("refresh carried scope=%q; that re-scopes the client and kills sibling tokens", gotForm.Get("scope"))
	}
	if gotForm.Get("client_id") != p.ClientID {
		t.Errorf("refresh client_id = %q", gotForm.Get("client_id"))
	}
}
