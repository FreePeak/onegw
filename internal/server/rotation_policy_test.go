package server

// #84 wiring end-to-end: the [rotation] / [providers.rotation] tables reach
// the live pools, a provider override wins over the global table, and a
// config with no rotation table keeps the shipped ladder exactly.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"onegw/internal/config"
)

func slowUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "rate_limit_exceeded", "message": "slow down",
		}})
	}))
}

// benchOf fires one request at model through h, then reports how long the
// provider's only account was benched (measured from the request start, so
// wall-clock drift between subtests cannot skew the window).
func benchOf(t *testing.T, h http.Handler, srv *Server, model, providerName string) time.Duration {
	t.Helper()
	start := time.Now()
	w := do(t, h, authed(t, model, "sk-test-gw"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("%s first hit: %d (%s)", model, w.Code, w.Body.String())
	}
	def, ok := srv.cur().pool.Get(providerName)
	if !ok {
		t.Fatalf("provider %s missing", providerName)
	}
	if def.AllInvalidated() {
		t.Fatal("rate limits are not billing refusals; nothing may be terminal")
	}
	_, ready := def.NextAccount("")
	if ready.IsZero() {
		t.Fatal("no bench recorded after a 429")
	}
	return ready.Sub(start)
}

func TestRotationPolicyWiredFromConfig(t *testing.T) {
	up := slowUpstream()
	defer up.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Rotation = config.RotationCfg{CooldownBase: "1s", CooldownCap: "2s"}
	cfg.Providers = []config.ProviderCfg{
		{Name: "tuned", Kind: "openai", BaseURL: up.URL, APIKey: "k", Models: []string{"tuned/m"},
			Rotation: config.RotationCfg{CooldownBase: "500ms"}},
		{Name: "global", Kind: "openai", BaseURL: up.URL, APIKey: "k", Models: []string{"global/m"}},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// Provider override beats the global table; the untouched provider gets
	// the global value.
	if d := benchOf(t, h, srv, "tuned/m", "tuned"); d > 1500*time.Millisecond {
		t.Fatalf("tuned ladder must start at its 500ms override, got %v", d)
	}
	if d := benchOf(t, h, srv, "global/m", "global"); d > 1500*time.Millisecond {
		t.Fatalf("global ladder must start at the configured 1s, got %v", d)
	}

	// No [rotation] table anywhere: the shipped 10s base still governs — the
	// byte-identical-behaviour guarantee for every existing config.
	stock := &config.Config{}
	stock.Server.DataDir = "memory"
	stock.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	stock.Providers = []config.ProviderCfg{
		{Name: "stock", Kind: "openai", BaseURL: up.URL, APIKey: "k", Models: []string{"stock/m"}},
	}
	stock.Defaults()
	if err := stock.Validate(); err != nil {
		t.Fatalf("stock config: %v", err)
	}
	srv2, err := New(stock)
	if err != nil {
		t.Fatalf("new server 2: %v", err)
	}
	defer srv2.Close()
	if d := benchOf(t, srv2.Handler(), srv2, "stock/m", "stock"); d < 9*time.Second || d > 11*time.Second {
		t.Fatalf("absent [rotation] must keep the shipped 10s base, got %v", d)
	}
}
