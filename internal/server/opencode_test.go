package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"onegw/internal/config"
)

// TestOpencodeRotationAndSession proves the two opencode integration
// invariants end-to-end through the real pipeline: requests rotate across
// the configured keys, and every upstream call carries an x-opencode-session.
func TestOpencodeRotationAndSession(t *testing.T) {
	var mu sync.Mutex
	seenKeys := map[string]int{}
	missingSession := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seenKeys[r.Header.Get("Authorization")]++
		if r.Header.Get("X-Opencode-Session") == "" {
			missingSession++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-1", "object": "chat.completion", "model": "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"}}},
		})
	}))
	defer up.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.Keys = []string{"gw-key"}
	cfg.Saver.Enabled = false
	cfg.Providers = []config.ProviderCfg{{
		Name: "opencode", Kind: "opencode", BaseURL: up.URL,
		Keys: []string{"oc-key-1", "oc-key-2"},
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// Bare model resolution: "glm-5.2" must route to the opencode provider
	// even though the config lists no explicit models (default catalog).
	for range 4 {
		r := chatReq(t, "glm-5.2")
		r.Header.Set("Authorization", "Bearer gw-key")
		w := do(t, s.Handler(), r)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if missingSession != 0 {
		t.Fatalf("%d upstream calls missing x-opencode-session", missingSession)
	}
	if len(seenKeys) != 2 {
		t.Fatalf("rotation used %d distinct keys (%v), want both", len(seenKeys), seenKeys)
	}
	for k, n := range seenKeys {
		if n != 2 {
			t.Fatalf("key %s served %d requests, want 2 (strict round-robin)", k, n)
		}
	}
}
