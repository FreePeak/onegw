package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	cfg.Auth.KeyList = []config.AuthKey{{Key: "gw-key"}}
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

// responsesStub mimics the OpenCode zen/go /v1/responses endpoint: it
// validates the Responses request shape and answers (SSE or JSON) like the
// real upstream.
func responsesStub(t *testing.T, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("grok must hit /v1/responses, got %s", r.URL.Path)
		}
		if r.Header.Get("X-Opencode-Session") == "" {
			t.Error("responses call missing session header")
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req["model"] != "grok-4.6" {
			t.Errorf("upstream model = %v, want grok-4.6 (provider prefix stripped)", req["model"])
		}
		if _, ok := req["messages"]; ok {
			t.Error("responses body must not carry chat-completions messages")
		}
		if _, ok := req["input"]; !ok {
			t.Error("responses body missing input")
		}
		if req["store"] != false {
			t.Errorf("store = %v, want false", req["store"])
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.6\"}}\n\n")
			fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n")
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"grok-4.6","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":7,"output_tokens":3}}`))
	}))
}

func opencodeGw(t *testing.T, up *httptest.Server) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "gw-key"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "opencode", Kind: "opencode", BaseURL: up.URL,
		Keys: []string{"oc-k1"}, Models: []string{"grok-4.6", "mimo-v2.5"},
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// OpenAI chat client -> Responses upstream, non-streaming: exercises the
// buffered cross-format path (decode Responses JSON -> unified -> encode
// OpenAI completion).
func TestGrokNonStreamViaResponsesUpstream(t *testing.T) {
	s := opencodeGw(t, responsesStub(t, false))
	r := chatReq(t, "opencode/grok-4.6")
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want JSON for non-stream", ct)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client got non-OpenAI-shaped body: %v (%s)", err, w.Body.String())
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "pong" {
		t.Fatalf("content missing: %s", w.Body.String())
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage not translated: %+v", resp.Usage)
	}
}

// OpenAI chat client -> Responses upstream, streaming: SSE re-encoded
// event-by-event into chat.completion.chunk.
func TestGrokStreamViaResponsesUpstream(t *testing.T) {
	s := opencodeGw(t, responsesStub(t, true))
	r := chatReq(t, "opencode/grok-4.6")
	r.Body = io.NopCloser(strings.NewReader(`{"model":"opencode/grok-4.6","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"pong"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream shape wrong: %s", body)
	}
}

// Anthropic client -> Responses upstream: the buffered path must answer in
// Anthropic Messages shape.
func TestGrokAnthropicClientViaResponses(t *testing.T) {
	s := opencodeGw(t, responsesStub(t, false))
	body, _ := json.Marshal(map[string]any{
		"model": "opencode/grok-4.6", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not anthropic-shaped: %v (%s)", err, w.Body.String())
	}
	if resp.Type != "message" || len(resp.Content) == 0 || resp.Content[0].Text != "pong" {
		t.Fatalf("anthropic content wrong: %s", w.Body.String())
	}
	if resp.Usage.InputTokens != 7 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

// Chat-capable models on the same provider must keep using
// /v1/chat/completions untouched (per-model routing does not leak).
func TestMimoStillChatCompletions(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"mimo-v2.5","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s := opencodeGw(t, up)
	r := chatReq(t, "opencode/mimo-v2.5")
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("mimo path = %s", gotPath)
	}
}
