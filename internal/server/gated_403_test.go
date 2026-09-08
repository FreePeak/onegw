package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
)

// gatedUpstream answers the gated-403 body for bearer "key-gated" while
// gateOn is true (a deposit flips it), a normal completion otherwise. It
// records every bearer key it saw.
type gatedUpstream struct {
	mu      sync.Mutex
	gateOn  bool
	seenKey []string
	srv     *httptest.Server
}

func newGatedUpstream(t *testing.T) *gatedUpstream {
	g := &gatedUpstream{gateOn: true}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		g.mu.Lock()
		g.seenKey = append(g.seenKey, auth)
		gate := g.gateOn
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if auth == "Bearer key-gated" && gate {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":{"message":"Access restricted. Deposit required to unlock premium models.","type":"access_denied","code":"access_denied"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gatedUpstream) keys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seenKey...)
}

func newTestServer2(t *testing.T, cfg *config.Config) (*Server, http.Handler) {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, srv.Handler()
}

// gatedCfg builds a server config with named per-provider accounts.
func gatedCfg(t *testing.T, provs ...config.ProviderCfg) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-client"}}
	cfg.Providers = append(cfg.Providers, provs...)
	if len(provs) > 1 {
		cfg.Combos = []config.ComboCfg{{
			Name:    "pair",
			Targets: []string{provs[0].Name + "/" + provs[0].Models[0], provs[1].Name + "/" + provs[1].Models[0]},
		}}
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func authSk(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer sk-client")
	return r
}

// A gated 403 must rotate to the pool's healthy account inside the same
// request: the client gets the completion, never the 403, and the gated
// key is benched (it stops being picked while the gate holds).
func TestGated403RotatesToHealthyAccount(t *testing.T) {
	g := newGatedUpstream(t)
	srv, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: g.srv.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "gated", APIKey: "key-gated"}, {Name: "ok", APIKey: "key-ok"}},
	}))

	w := do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("got %d %s, want completion served by the healthy account", w.Code, w.Body.String())
	}
	keys := g.keys()
	if len(keys) != 2 || keys[0] != "Bearer key-gated" || keys[1] != "Bearer key-ok" {
		t.Fatalf("upstream saw %v, want gated then healthy", keys)
	}

	// The gated key is benched: the next request goes straight to the
	// healthy account (one upstream call only).
	g.seenKey = nil
	w = do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != 200 {
		t.Fatalf("follow-up request: %d %s", w.Code, w.Body.String())
	}
	if keys = g.keys(); len(keys) != 1 || keys[0] != "Bearer key-ok" {
		t.Fatalf("benched gated key was re-picked: %v", keys)
	}
	_ = srv
}

// A provider whose whole pool is gated must fall through the combo to the
// next target (requirement: the client never sees the gated 403 while
// another target succeeds).
func TestGated403ComboFallsThrough(t *testing.T) {
	g := newGatedUpstream(t)
	ok := upstreamStub("m2")
	defer ok.Close()
	_, h := newTestServer2(t, gatedCfg(t,
		config.ProviderCfg{
			Name: "p1", Kind: "openai", BaseURL: g.srv.URL, Models: []string{"m1"},
			Accounts: []config.Acct{{Name: "gated", APIKey: "key-gated"}},
		},
		config.ProviderCfg{
			Name: "p2", Kind: "openai", BaseURL: ok.URL, Models: []string{"m2"},
			Accounts: []config.Acct{{Name: "ok", APIKey: "key-ok"}},
		},
	))

	w := do(t, h, authSk(chatReq(t, "pair")))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m2") {
		t.Fatalf("got %d %s, want fall-through to p2", w.Code, w.Body.String())
	}
}

// A fully-gated single-target pool answers the cooling-pool 429 + Retry-After
// (NOT the 403, and NOT the 503 quota answer — gating is not quota).
func TestGatedPoolAnswers429Not503(t *testing.T) {
	g := newGatedUpstream(t)
	srv, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: g.srv.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "gated", APIKey: "key-gated"}},
	}))
	_ = srv

	// First request benches the account after its 403 and answers 429.
	w := do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d %s, want 429 for a fully-gated pool", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Deposit required") || strings.Contains(w.Body.String(), "quota") {
		t.Fatalf("gated 403 or quota answer leaked: %s", w.Body.String())
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("error shape: %s (err=%v)", w.Body.String(), err)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After missing on gated-pool 429")
	}

	// With the pool benched, a retry must NOT reach the upstream (the
	// cooling-pool fast-fail) and still answers 429 — quota stays silent
	// because the provider was never quota-exhausted.
	n := len(g.keys())
	w = do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != http.StatusTooManyRequests || len(g.keys()) != n {
		t.Fatalf("retry hit upstream during bench: %d %s", w.Code, w.Body.String())
	}
}

// Non-gated 403s keep failing fast: the client sees the upstream 403.
func TestNonGated403Surfaces(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(up.Close)
	_, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "bad", APIKey: "key-invalid"}},
	}))
	w := do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d %s, want the 403 surfaced (fail fast)", w.Code, w.Body.String())
	}
}

// Streaming fast path: a pre-body gated 403 must answer the cooling-pool
// 429 (never the 403), bench the account, and the client's retry lands on
// the healthy account.
func TestStreamGated403Answers429ThenRotates(t *testing.T) {
	g := newGatedUpstream(t)
	cfg := streamCfg(t, false, nil, providerSpec{name: "p1", up: g.srv.URL, model: "m1"})
	// streamCfg/makeCfg build a single-key provider; add the second
	// (healthy) account directly.
	cfg.Providers[0].Accounts = []config.Acct{
		{Name: "gated", APIKey: "key-gated"}, {Name: "ok", APIKey: "key-ok"},
	}
	_, h := newStreamServer(t, cfg)

	body := `{"model":"p1/m1","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, func(r *http.Request) *http.Request { r.Header.Set("Authorization", "Bearer sk-test-key"); return r }(r))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("stream gated 403: got %d %s, want 429", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Deposit required") {
		t.Fatalf("gated 403 leaked to a stream client: %s", w.Body.String())
	}

	// Retry: the benched key is skipped and the healthy one serves.
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Authorization", "Bearer sk-test-key")
	w2 := do(t, h, r2)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "ok") {
		t.Fatalf("stream retry: got %d %s, want completion from the healthy account", w2.Code, w2.Body.String())
	}
}
