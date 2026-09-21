package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
	"onegw/internal/provider"
)

// chatCompletionStub answers one OpenAI chat completion and records the
// bearer credential the gateway picked for each call.
func keyRecordingStub(t *testing.T, seen *[]string, failKey string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		*seen = append(*seen, key)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if key == failKey {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-x", "object": "chat.completion", "model": "m1",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"}}},
		})
	}))
}

// stickyCfg builds a two-account provider with sticky affinity and one
// client auth key.
func stickyCfg(t *testing.T, up string, sticky string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-client", Name: "omp"}}
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up, Sticky: sticky,
		Models: []string{"m1"},
		Accounts: []config.Acct{
			{Name: "a", APIKey: "key-a"},
			{Name: "b", APIKey: "key-b"},
		},
	})
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func withSession(t *testing.T, session string) *http.Request {
	t.Helper()
	r := chatReq(t, "m1")
	r.Header.Set("Authorization", "Bearer sk-client")
	if session != "" {
		r.Header.Set(provider.OpenCodeSessionHeader, session)
	}
	return r
}

func TestStickyAffinityPinsSameAccountPerIdentity(t *testing.T) {
	var seen []string
	up := keyRecordingStub(t, &seen, "")
	defer up.Close()
	srv, err := New(stickyCfg(t, up.URL, "1h"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	// Same identity twice: the pinned account must serve both.
	if w := do(t, h, withSession(t, "sess-1")); w.Code != 200 {
		t.Fatalf("request 1: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, withSession(t, "sess-1")); w.Code != 200 {
		t.Fatalf("request 2: %d %s", w.Code, w.Body.String())
	}
	if len(seen) != 2 || seen[0] != seen[1] {
		t.Fatalf("same identity must reuse one account, saw %v", seen)
	}

	// A second identity lands on the other account and sticks to it.
	if w := do(t, h, withSession(t, "sess-2")); w.Code != 200 {
		t.Fatalf("request 3: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, withSession(t, "sess-2")); w.Code != 200 {
		t.Fatalf("request 4: %d %s", w.Code, w.Body.String())
	}
	if seen[2] != seen[3] || seen[2] == seen[0] {
		t.Fatalf("second identity must pin the other account, saw %v", seen)
	}
}

func TestStickyUnpinsAfterFailedAttempt(t *testing.T) {
	var seen []string
	up := keyRecordingStub(t, &seen, "key-a") // account a is dead
	defer up.Close()
	srv, err := New(stickyCfg(t, up.URL, "1h"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	// First request: key-a 500 → unpin → retry lands on key-b → 200.
	if w := do(t, h, withSession(t, "sess-1")); w.Code != 200 {
		t.Fatalf("failed account must be retried on the other: %d %s", w.Code, w.Body.String())
	}
	if len(seen) != 2 || seen[0] != "key-a" || seen[1] != "key-b" {
		t.Fatalf("failover sequence wrong: %v", seen)
	}

	// Second request: pinned to the account that survived (key-b), no new
	// dead-key attempt.
	if w := do(t, h, withSession(t, "sess-1")); w.Code != 200 {
		t.Fatalf("second request: %d %s", w.Code, w.Body.String())
	}
	if len(seen) != 3 || seen[2] != "key-b" {
		t.Fatalf("pin must move to the healthy account, saw %v", seen)
	}
}

func TestStickyOffStillPinsConversation(t *testing.T) {
	var seen []string
	up := keyRecordingStub(t, &seen, "")
	defer up.Close()
	srv, err := New(stickyCfg(t, up.URL, ""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	for range 4 {
		if w := do(t, h, withSession(t, "sess-1")); w.Code != 200 {
			t.Fatalf("request: %d", w.Code)
		}
	}
	if len(seen) != 4 || seen[0] != seen[1] || seen[1] != seen[2] || seen[2] != seen[3] {
		t.Fatalf("session header must pin without sticky, saw %v", seen)
	}
}

func TestConversationFingerprintPinsToolLoop(t *testing.T) {
	var seen []string
	up := keyRecordingStub(t, &seen, "")
	defer up.Close()
	srv, err := New(stickyCfg(t, up.URL, ""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	first := chatWithMessages(t, "m1", []any{
		map[string]any{"role": "user", "content": "look at foo.go"},
	})
	cont := chatWithMessages(t, "m1", []any{
		map[string]any{"role": "user", "content": "look at foo.go"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "package foo"},
	})
	other := chatWithMessages(t, "m1", []any{
		map[string]any{"role": "user", "content": "a different task"},
	})
	for _, r := range []*http.Request{first, cont, other} {
		r.Header.Set("Authorization", "Bearer sk-client")
	}
	if w := do(t, h, first); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, cont); w.Code != 200 {
		t.Fatalf("cont: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, other); w.Code != 200 {
		t.Fatalf("other: %d %s", w.Code, w.Body.String())
	}
	if len(seen) != 3 || seen[0] != seen[1] {
		t.Fatalf("tool-loop continuation must reuse the first-turn account, saw %v", seen)
	}
	if seen[2] == seen[0] {
		t.Fatalf("a different first user turn must rotate, saw %v", seen)
	}
}

func TestAuthKeyIdentityStillRotatesWithoutSticky(t *testing.T) {
	var seen []string
	up := keyRecordingStub(t, &seen, "")
	defer up.Close()
	srv, err := New(stickyCfg(t, up.URL, ""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	for range 4 {
		r := chatWithMessages(t, "m1", []any{
			map[string]any{"role": "assistant", "content": "no user turn"},
		})
		r.Header.Set("Authorization", "Bearer sk-client")
		if w := do(t, h, r); w.Code != 200 {
			t.Fatalf("request: %d", w.Code)
		}
	}
	if seen[0] == seen[1] || seen[1] == seen[2] || seen[2] == seen[3] {
		t.Fatalf("k: identity without sticky must rotate, saw %v", seen)
	}
}

func chatWithMessages(t *testing.T, model string, messages []any) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "messages": messages})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	return r
}
