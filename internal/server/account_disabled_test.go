package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"onegw/internal/config"
)

// TestDisabledAccountNeverCalled: an exhausted key marked disabled = true
// must not receive any upstream attempt, even when it is listed first.
// Regression for the live opencode config where operators "disabled" a
// burned account and the gateway kept rotating onto it after reload.
func TestDisabledAccountNeverCalled(t *testing.T) {
	var hits atomic.Int64
	var lastAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s := newCredentialCfg(t, config.ProviderCfg{
		Name: "opencode", Kind: "openai", BaseURL: up.URL,
		Models: []string{"m"},
		Accounts: []config.Acct{
			{Name: "exhausted", APIKey: "sk-dead", Disabled: true},
			{Name: "live", APIKey: "sk-live"},
		},
	})
	gwChat(t, s, "opencode/m")

	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	if lastAuth != "Bearer sk-live" {
		t.Fatalf("Authorization = %q, want the live account only", lastAuth)
	}
	def, ok := s.cur().pool.Get("opencode")
	if !ok {
		t.Fatal("provider missing")
	}
	if len(def.Accounts) != 1 || def.Accounts[0].Name != "live" {
		t.Fatalf("pool accounts = %+v, want only live", def.Accounts)
	}
}

// TestEmptyAccountShellNeverCalled: a half-commented
// [[providers.accounts]] header decodes as {Name:"",APIKey:""}. That shell
// must not enter the pool (and must not inherit the provider key).
func TestEmptyAccountShellNeverCalled(t *testing.T) {
	var hits atomic.Int64
	var lastAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	s := newCredentialCfg(t, config.ProviderCfg{
		Name: "opencode", Kind: "openai", BaseURL: up.URL, APIKey: "provider-level",
		Models: []string{"m"},
		Accounts: []config.Acct{
			{}, // empty shell
			{Name: "kton2", APIKey: "sk-k2"},
		},
	})
	gwChat(t, s, "opencode/m")

	if hits.Load() != 1 || lastAuth != "Bearer sk-k2" {
		t.Fatalf("hits=%d auth=%q, want 1x sk-k2 (empty shell must not inherit provider-level)", hits.Load(), lastAuth)
	}
}
