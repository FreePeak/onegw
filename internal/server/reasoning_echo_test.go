package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// reasoningEchoErr is the live tokenharbor 400 body (verbatim modulo
// formatting): backticks inside, so one interpreted-string const serves
// every stub.
const reasoningEchoErr = "{\"error\":{\"message\":\"The `reasoning_content` in the thinking mode must be passed back to the API.\",\"type\":\"AI_APICallError\"}}"

// reasoningEchoStub mirrors the tokenharbor DeepSeek contract: requests
// whose assistant history carries thinking under a "reasoning" key (AI SDK
// serialization) are rejected with the live 400 — while the same body with
// the echo named reasoning_content serves. It records every body.
type reasoningEchoStub struct {
	mu   sync.Mutex
	srv  *httptest.Server
	bods [][]byte
}

func newReasoningEchoStub() *reasoningEchoStub {
	ts := &reasoningEchoStub{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(b, &req)
		bad := false
		for _, m := range req.Messages {
			if m["role"] == "assistant" {
				if _, has := m["reasoning"]; has {
					bad = true
				}
			}
		}
		ts.mu.Lock()
		ts.bods = append(ts.bods, b)
		ts.mu.Unlock()
		if bad {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(reasoningEchoErr))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-ok", "object": "chat.completion", "model": "deepseek-v4.1-flash",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"}}},
		})
	}))
	return ts
}

func (ts *reasoningEchoStub) bodies() [][]byte {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([][]byte(nil), ts.bods...)
}

func reasoningReq(t *testing.T, model string, stream bool) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "What is 2+2?"},
			map[string]any{"role": "assistant", "content": "", "reasoning": "2+2 is basic arithmetic.",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{\"command\":\"ls\"}"}}}},
			map[string]any{"role": "tool", "content": "file1\nfile2", "tool_call_id": "c1"},
			map[string]any{"role": "user", "content": "Now multiply by 3"},
		},
		"reasoning_effort":      "low",
		"stream":                stream,
		"max_completion_tokens": 128,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	return r
}

// TestReasoningEchoRewrite pins the same-format fix: omp (AI SDK) sends
// assistant thinking as "reasoning"; the DeepSeek-dialect upstream demands
// it echoed as "reasoning_content". The gateway must rename it before the
// upstream call, so a body that failed live on 2026-09-10 now serves —
// and the combo never needed its second leg.
func TestReasoningEchoRewrite(t *testing.T) {
	st := newReasoningEchoStub()
	defer st.srv.Close()
	other, otherCap := captureStub()
	defer other.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "th", up: st.srv.URL, model: "deepseek-v4.1-flash:free"},
		providerSpec{name: "oc", up: other.URL, model: "mimo-v2.5"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	w := do(t, h, reasoningReq(t, "pair", false))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("reasoning-echo body must serve after rename: code=%d body=%s", w.Code, w.Body.String())
	}
	bods := st.bodies()
	if len(bods) != 1 {
		t.Fatalf("upstream hits=%d, want 1", len(bods))
	}
	if !strings.Contains(string(bods[0]), `"reasoning_content":"2+2 is basic arithmetic."`) {
		t.Fatalf("upstream body must carry reasoning_content, got %s", bods[0])
	}
	if strings.Contains(string(bods[0]), `"reasoning":"2+2 is basic arithmetic."`) {
		t.Fatalf("reasoning key must be renamed away, got %s", bods[0])
	}
	// The rewrite satisfied the first leg: the fallback leg never saw it.
	if b, _, _ := otherCap.snapshot(); len(b) != 0 {
		t.Fatalf("combo must not need the fallback leg once renamed, got %s", b)
	}
}

// TestReasoningEchoNullDropped pins the null case: AI SDK emits
// "reasoning":null on assistant turns without thinking. Renaming a null
// would send reasoning_content:null; the key must simply disappear.
func TestReasoningEchoNullDropped(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "th/deepseek-v4.1-flash:free",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello", "reasoning": nil},
			map[string]any{"role": "user", "content": "again"},
		},
	})
	out, err := normalizeRoles(body)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	_ = json.Unmarshal(out, &root)
	msgs := root["messages"].([]any)
	m := msgs[1].(map[string]any)
	if _, has := m["reasoning"]; has {
		t.Fatalf("null reasoning must be dropped, got %s", out)
	}
	if _, has := m["reasoning_content"]; has {
		t.Fatalf("null reasoning must not become reasoning_content, got %s", out)
	}
}

// TestReasoningEchoDetailsRenamed pins the reasoning_details alias: pi
// replays the structured reasoning_details[] array (openai-completions.js
// :1043) for commandcode-served turns; a DeepSeek-dialect upstream needs
// the native reasoning_content key. Converted only when reasoning_content
// is absent; when the native echo is already present the details stay.
func TestReasoningEchoDetailsRenamed(t *testing.T) {
	st := newReasoningEchoStub()
	defer st.srv.Close()
	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "th", up: st.srv.URL, model: "deepseek-v4.1-flash:free"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{
		"model": "th/deepseek-v4.1-flash:free",
		"messages": []any{
			map[string]any{"role": "user", "content": "What is 2+2?"},
			map[string]any{"role": "assistant", "content": "4", "reasoning_details": []any{
				map[string]any{"type": "reasoning.text", "text": "2+2 is basic arithmetic.", "format": "unknown", "index": 0},
			}},
		},
		"stream": false,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	w := do(t, h, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("details-bearing body must serve after rename, got code=%d body=%s", w.Code, w.Body.String())
	}
	bods := st.bodies()
	if len(bods) != 1 {
		t.Fatalf("upstream hits=%d, want 1", len(bods))
	}
	s := string(bods[0])
	if !strings.Contains(s, `"reasoning_content":"2+2 is basic arithmetic."`) {
		t.Fatalf("upstream body must carry native echo, got %s", s)
	}
	if strings.Contains(s, "reasoning_details") {
		t.Fatalf("converted alias key must be deleted, got %s", s)
	}

	// Native echo wins: details untouched when reasoning_content is present.
	body2, _ := json.Marshal(map[string]any{
		"model": "th/deepseek-v4.1-flash:free",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "Hello", "reasoning_content": "native", "reasoning_details": []any{
				map[string]any{"type": "reasoning.text", "text": "detail"},
			}},
		},
		"stream": false,
	})
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body2))
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Authorization", "Bearer sk-test-key")
	if w2 := do(t, h, r2); w2.Code != 200 {
		t.Fatalf("native-echo body must serve, got code=%d", w2.Code)
	}
	bods2 := st.bodies()
	if !strings.Contains(string(bods2[len(bods2)-1]), `"reasoning_details":[{"`) {
		t.Fatalf("details must stay untouched when native echo present, got %s", bods2[len(bods2)-1])
	}
}

// TestReasoningEcho400DirectRouteTerminal pins the direct-route contract:
// with no next target, the honest upstream 400 must surface (the client
// sees the real refusal), not a masked 503.
func TestReasoningEcho400DirectRouteTerminal(t *testing.T) {
	th := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(reasoningEchoErr))
	}))
	defer th.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "th", up: th.URL, model: "deepseek-v4.1-flash:free"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	w := do(t, h, reasoningReq(t, "th/deepseek-v4.1-flash:free", false))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "reasoning_content") {
		t.Fatalf("direct route must surface the upstream 400, got code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestStreamRelayReasoningBuffered pins the streaming fast-path gate: a
// reasoning-bearing body must not ride the single-shot relay (the rename
// needs the buffered path). The upstream still sees the renamed key.
func TestStreamRelayReasoningBuffered(t *testing.T) {
	st := newReasoningEchoStub()
	defer st.srv.Close()
	cfg := streamCfg(t, false, nil, providerSpec{name: "th", up: st.srv.URL, model: "deepseek-v4.1-flash:free"})
	_, h := newStreamServer(t, cfg)

	w := do(t, h, reasoningReq(t, "th/deepseek-v4.1-flash:free", true))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("streaming reasoning-echo body must serve buffered: code=%d body=%s", w.Code, w.Body.String())
	}
	bods := st.bodies()
	if len(bods) != 1 {
		t.Fatalf("upstream hits=%d, want 1", len(bods))
	}
	if !strings.Contains(string(bods[0]), `"reasoning_content":"2+2 is basic arithmetic."`) {
		t.Fatalf("streamed upstream body must carry reasoning_content, got %s", bods[0])
	}
}
