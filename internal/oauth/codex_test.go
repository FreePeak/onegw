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

func TestCodexAccountIDAndPlanDecodeOffIDToken(t *testing.T) {
	claims := map[string]any{
		"email": "someone@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "workspace-abc",
			"chatgpt_plan_type":  "plus",
		},
	}
	// The ID token is where ChatGPT puts these (token_data.rs: IdClaims) —
	// an access token that happens to carry them is the fallback, not the
	// primary source.
	idTok := Token{AccessToken: "opaque-access-token", IDToken: codexTestJWT(t, claims)}
	if got := CodexAccountID(idTok); got != "workspace-abc" {
		t.Errorf("CodexAccountID(id_token) = %q, want workspace-abc", got)
	}
	if got := CodexPlan(idTok); got != "plus" {
		t.Errorf("CodexPlan(id_token) = %q, want plus", got)
	}
	// The access token is still honoured when the id_token has nothing (a
	// store written before id_token was kept).
	accessOnly := Token{AccessToken: codexTestJWT(t, claims)}
	if got := CodexAccountID(accessOnly); got != "workspace-abc" {
		t.Errorf("CodexAccountID(access_token) = %q, want workspace-abc", got)
	}
	// id_token wins over a stale access token, so a workspace switch cannot
	// send two different ids upstream from one account.
	mixed := Token{
		AccessToken: codexTestJWT(t, map[string]any{
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "OLD-workspace"},
		}),
		IDToken: codexTestJWT(t, claims),
	}
	if got := CodexAccountID(mixed); got != "workspace-abc" {
		t.Errorf("CodexAccountID(mixed) = %q, want the id_token's workspace-abc", got)
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
		for _, shape := range []Token{{AccessToken: tok}, {IDToken: tok}} {
			if got := CodexAccountID(shape); got != "" {
				t.Errorf("%s: CodexAccountID = %q, want empty", name, got)
			}
			if got := CodexPlan(shape); got != "" {
				t.Errorf("%s: CodexPlan = %q, want empty", name, got)
			}
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

// The whole reason the id_token is kept: ChatGPT issues it, and the claims
// onegw needs are ONLY in it. Dropping it at exchange time would leave the
// workspace unknown and every request would go out without the id ChatGPT
// binds its plan to — a silent 403, not an error.
func TestCodexExchangeKeepsIDToken(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		claims := codexTestJWT(t, map[string]any{
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "workspace-live",
				"chatgpt_plan_type":  "pro",
			},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-opaque","id_token":"` + claims + `",` +
			`"refresh_token":"rt-1","expires_in":3600}`))
	}))
	defer srv.Close()

	p, _ := Lookup("codex")
	p.TokenURL = srv.URL
	tok, err := p.ExchangeCode(t.Context(), nil, "the-code", CodexRedirectURI, "verifier-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.IDToken == "" {
		t.Fatal("the id_token was discarded at exchange — the workspace claim is lost")
	}
	if got := CodexAccountID(*tok); got != "workspace-live" {
		t.Errorf("workspace off the exchanged pair = %q, want workspace-live", got)
	}
	if got := CodexPlan(*tok); got != "pro" {
		t.Errorf("plan off the exchanged pair = %q, want pro", got)
	}
	_ = gotForm
}

// A refresh that does NOT re-issue the id_token must not erase the stored
// one: dropping the claims there would strand the account on an unknown
// workspace mid-session, hours before its next real login.
func TestCodexRefreshKeepsIDTokenWhenVendorOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`))
	}))
	defer srv.Close()

	mgr := NewManager(NewTokenStore("memory"))
	defer mgr.Stop()
	p, _ := Lookup("codex")
	p.TokenURL = srv.URL
	spec := AccountSpec{Key: "codex/main", Provider: p}
	claims := codexTestJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-keep"},
	})
	if err := mgr.store.Put(spec.Key, Token{AccessToken: "at-1", IDToken: claims,
		RefreshToken: "rt-1", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	mgr.Sync([]AccountSpec{spec})
	refreshed, err := mgr.refresh(t.Context(), spec)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.AccessToken != "at-2" {
		t.Errorf("access token = %q, want at-2", refreshed.AccessToken)
	}
	if got := CodexAccountID(*refreshed); got != "ws-keep" {
		t.Errorf("workspace after a refresh that omitted the id_token = %q, want ws-keep", got)
	}
}
