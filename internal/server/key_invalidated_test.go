package server

// #80 end-to-end: an upstream billing refusal (402/insufficient_quota) takes
// the credential out of rotation permanently — no ladder re-offers it, the
// sibling account serves, the ring records one key_invalidated row, the
// dashboard view exposes the state, and only an operator reset (or a rotated
// key) brings the account back.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"onegw/internal/config"
)

// billingUpstream answers 402 for one specific bearer key and 200 for others,
// counting hits per key.
type billingUpstream struct {
	srv  *httptest.Server
	bad  string // api key that gets the billing refusal
	fail atomic.Int64
	ok   atomic.Int64
}

func newBillingUpstream(badKey string) *billingUpstream {
	b := &billingUpstream{bad: badKey}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		if key == b.bad {
			b.fail.Add(1)
			w.WriteHeader(http.StatusPaymentRequired)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "insufficient_quota", "message": "Insufficient balance: add credits",
			}})
			return
		}
		b.ok.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"}}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	return b
}

func billingCfg(up *billingUpstream, accounts []config.Acct) *config.Config {
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "p", Kind: "openai", BaseURL: up.srv.URL, Models: []string{"p/m"}, Accounts: accounts,
	}}
	cfg.Defaults()
	return cfg
}

func TestTerminalKeyInvalidationOnBillingRefusal(t *testing.T) {
	up := newBillingUpstream("sk-dead")
	defer up.srv.Close()
	cfg := billingCfg(up, []config.Acct{{Name: "dead", APIKey: "sk-dead"}, {Name: "live", APIKey: "sk-live"}})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// First request: the dead account is tried once, invalidated, and the
	// sibling serves without a second doomed attempt.
	w := do(t, h, authed(t, "p/m", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("first request: want 200 via the live account, got %d (%s)", w.Code, w.Body.String())
	}
	if got := up.fail.Load(); got != 1 {
		t.Fatalf("dead key hits after first request = %d, want exactly 1", got)
	}

	// Second request: the terminal account is skipped entirely.
	w = do(t, h, authed(t, "p/m", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("second request: %d (%s)", w.Code, w.Body.String())
	}
	if got := up.fail.Load(); got != 1 {
		t.Fatalf("dead key hits after second request = %d — the invalidated account was re-offered", got)
	}
	if got := up.ok.Load(); got != 2 {
		t.Fatalf("live key hits = %d, want 2", got)
	}

	// The ring names the invalidation exactly once (log-once contract).
	var inval int
	for _, e := range srv.reqlog.latest(64) {
		if e.Kind == "key_invalidated" {
			inval++
			if e.Account != "dead" || e.Provider != "p" {
				t.Fatalf("ring row names the wrong actor: %+v", e)
			}
		}
	}
	if inval != 1 {
		t.Fatalf("key_invalidated rows = %d, want 1", inval)
	}

	// Dashboard view exposes the terminal state (providers edit view).
	st := srv.cur()
	def, _ := st.pool.Get("p")
	if names := def.Invalidated(); len(names) != 1 || names[0] != "dead" {
		t.Fatalf("pool Invalidated() = %v, want [dead]", names)
	}

	// Operator reset: 409 before invalidation is cleared? No — it is
	// invalidated, so the reset succeeds and the account is offered again
	// (the upstream still refuses, so it is re-invalidated on the next use).
	resetReq := httptest.NewRequest(http.MethodPost, "/admin/api/v1/providers/p/accounts/dead/reset", nil)
	resetReq.Header.Set("X-Admin-Password", "admin")
	w = do(t, h, resetReq)
	if w.Code != http.StatusOK {
		t.Fatalf("reset: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if def.AllInvalidated() || len(def.Invalidated()) != 0 {
		t.Fatalf("after reset the account must be active again: %v", def.Invalidated())
	}

	// Resetting an account that is not invalidated is an honest 409, not a
	// silent success.
	w = do(t, h, resetReq)
	if w.Code != http.StatusConflict {
		t.Fatalf("reset of a healthy account: want 409, got %d (%s)", w.Code, w.Body.String())
	}

	// Reset re-offers the account: round-robin must reach it again, and while
	// the vendor still refuses it, it is re-invalidated (one more transition
	// row) rather than retried forever.
	w = do(t, h, authed(t, "p/m", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("post-reset request 1: %d (%s)", w.Code, w.Body.String())
	}
	w = do(t, h, authed(t, "p/m", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("post-reset request 2: %d (%s)", w.Code, w.Body.String())
	}
	if got := up.fail.Load(); got != 2 {
		t.Fatalf("dead key hits after reset = %d, want 2 (reset must re-offer it once)", got)
	}
	if names := def.Invalidated(); len(names) != 1 || names[0] != "dead" {
		t.Fatalf("the still-refused key must be terminal again: %v", names)
	}
	// With the balance fixed upstream, a reset brings the key back for good.
	up.bad = "sk-none"
	w = do(t, h, resetReq)
	if w.Code != http.StatusOK {
		t.Fatalf("second reset: %d (%s)", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		if w := do(t, h, authed(t, "p/m", "sk-test-gw")); w.Code != http.StatusOK {
			t.Fatalf("request after healing: %d (%s)", w.Code, w.Body.String())
		}
	}
	if got := up.fail.Load(); got != 2 {
		t.Fatalf("healed key was refused again (hits=%d)", got)
	}
	if n := len(def.Invalidated()); n != 0 {
		t.Fatalf("no account should be terminal now, got %v", def.Invalidated())
	}
}

// A pool whose only account is terminal answers the billing truth (503
// insufficient_quota naming the accounts), not a rate-limit lie.
func TestAllAccountsTerminalAnswersUnfunded(t *testing.T) {
	up := newBillingUpstream("sk-dead")
	defer up.srv.Close()
	cfg := billingCfg(up, []config.Acct{{Name: "only", APIKey: "sk-dead"}})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	w := do(t, h, authed(t, "p/m", "sk-test-gw"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 unfunded, got %d (%s)", w.Code, w.Body.String())
	}
	b := errBody(t, w)
	if b["code"] != "insufficient_quota" || b["type"] != "provider_accounts_unfunded" {
		t.Fatalf("unfunded error shape: %+v", b)
	}
	if msg, _ := b["message"].(string); !strings.Contains(msg, "only") || !strings.Contains(msg, "balance") {
		t.Fatalf("message must name the account and the remedy: %q", msg)
	}
	// The upstream is not hammered: the terminal account is never retried.
	if got := up.fail.Load(); got != 1 {
		t.Fatalf("dead key hits = %d, want 1", got)
	}
	w = do(t, h, authed(t, "p/m", "sk-test-gw"))
	if got := up.fail.Load(); got != 1 {
		t.Fatalf("second request re-burned the terminal key (hits=%d)", got)
	}
}
