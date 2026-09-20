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
	"onegw/internal/provider"
)

// opencodeEchoStub mirrors the Console Go contract proven live 2026-09-11
// (seq 2666, three-way bisect): a body whose LAST message is a tool result
// (pending tool-loop continuation) 400s when ANY assistant turn lacks
// reasoning_content; the same body with every assistant turn echoed serves.
// A trailing user turn disables the validation.
type opencodeEchoStub struct {
	mu   sync.Mutex
	srv  *httptest.Server
	bods [][]byte
}

func newOpencodeEchoStub() *opencodeEchoStub {
	ts := &opencodeEchoStub{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(b, &req)
		// The Console Go contract: a tool-tail continuation is refused
		// when ANY assistant turn lacks a non-empty echo. (Recomputed from
		// a clean base — an earlier shape left `bad` true for EVERY
		// tool-tail body, so the "serves after synthesis" assertions could
		// only ever pass through the combo's fallback leg.)
		bad := false
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1]["role"] == "tool" {
			for _, m := range req.Messages {
				if m["role"] != "assistant" {
					continue
				}
				if s, _ := m["reasoning_content"].(string); s == "" {
					bad = true
					break
				}
			}
		}
		ts.mu.Lock()
		ts.bods = append(ts.bods, b)
		ts.mu.Unlock()
		if bad {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`))
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

func (ts *opencodeEchoStub) bodies() [][]byte {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([][]byte(nil), ts.bods...)
}

// toolLoopReq builds the live failure shape: a history mixing turns WITH
// reasoning (served by thinking legs) and turns WITHOUT (served by
// non-thinking combo legs), ending on the tool result of the last
// assistant's tool call.
func toolLoopReq(t *testing.T, model string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "read it"},
			map[string]any{"role": "assistant", "content": "", "reasoning_content": "I should read the file.",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "content": "out1", "tool_call_id": "c1"},
			// cross-leg turn: served by a non-thinking provider, no echo at all
			map[string]any{"role": "assistant", "content": "Now grep:",
				"tool_calls": []any{map[string]any{"id": "c2", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "content": "out2", "tool_call_id": "c2"},
		},
		"stream":                false,
		"max_completion_tokens": 64,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "bash", "description": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	return r
}

// TestReasoningEchoSynthesizedOnToolLoop pins the opencode/deepseek 400 fix
// (live seq 2666): with echo_reasoning configured, a tool-loop continuation
// carrying echo-less assistant turns (turns other combo legs served without
// reasoning) must have the echo synthesized and serve — no 400, no fallback
// leg burned.
func TestReasoningEchoSynthesizedOnToolLoop(t *testing.T) {
	st := newOpencodeEchoStub()
	defer st.srv.Close()
	other := upstreamStub("mimo-v2.5")
	defer other.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-key"}}
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name: "oc", Kind: "openai", BaseURL: st.srv.URL, APIKey: "up-key",
		Models: []string{"deepseek-v4.1-flash"}, EchoReasoning: []string{"deepseek-*"},
	})
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name: "fb", Kind: "openai", BaseURL: other.URL, APIKey: "up-key",
		Models: []string{"mimo-v2.5"},
	})
	cfg.Combos = []config.ComboCfg{{Name: "free", Targets: []string{
		"oc/deepseek-v4.1-flash", "fb/mimo-v2.5"}}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	w := do(t, srv.Handler(), toolLoopReq(t, "free"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("tool-loop body must serve after echo synthesis: code=%d body=%s", w.Code, w.Body.String())
	}
	bods := st.bodies()
	if len(bods) != 1 {
		t.Fatalf("upstream hits=%d, want 1 (no fallback burn)", len(bods))
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(bods[0], &got); err != nil {
		t.Fatal(err)
	}
	for i, m := range got.Messages {
		if m["role"] != "assistant" {
			continue
		}
		if s, _ := m["reasoning_content"].(string); s == "" {
			t.Fatalf("assistant[%d] reached upstream without echo: %s", i, bods[0])
		}
	}
}

// TestReasoningEchoSynthesisScopesToLearnedModel pins the scoping: without
// echo_reasoning globs the contract is learned PER MODEL from the first
// refusal (that client's retry then serves), and a sibling model on the
// same provider stays untouched — its bodies never carry a synthesized
// echo, because the fill must not leak across models.
func TestReasoningEchoSynthesisScopesToLearnedModel(t *testing.T) {
	st := newOpencodeEchoStub()
	defer st.srv.Close()
	other, otherCap := captureStub()
	defer other.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-key"}}
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name: "oc", Kind: "openai", BaseURL: st.srv.URL, APIKey: "up-key",
		Models: []string{"deepseek-v4.1-flash", "mimo-v2.5"},
	})
	// A second provider carrying the sibling model, so the two requests
	// can never share learned state.
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name: "mm", Kind: "openai", BaseURL: other.URL, APIKey: "up-key",
		Models: []string{"mimo-v2.5"},
	})
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	// deepseek: refusal teaches, the bounded retry serves filled.
	w := do(t, srv.Handler(), toolLoopReq(t, "oc/deepseek-v4.1-flash"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("learned deepseek must serve on retry: code=%d body=%s", w.Code, w.Body.String())
	}
	if b, _, _ := otherCap.snapshot(); len(b) != 0 {
		t.Fatalf("deepseek must not touch the sibling leg, got %s", b)
	}

	// mimo: never learned, never configured — no synthesized echo may
	// appear in its upstream body even though the provider just learned
	// the contract for deepseek.
	w = do(t, srv.Handler(), toolLoopReq(t, "mm/mimo-v2.5"))
	if w.Code != 200 {
		t.Fatalf("sibling model must serve untouched: code=%d body=%s", w.Code, w.Body.String())
	}
	if b, _, _ := otherCap.snapshot(); strings.Contains(string(b), "(context elided)") {
		t.Fatalf("learn must not leak across models: %s", b)
	}
}

// TestReasoningEchoConfigWiring asserts the config→Def wiring: a
// providerSpec-built Def missed exactly this once (a replaced field line
// shipped green through every unit test because tests build Def literals
// directly) — so pin that the apply path copies ProviderCfg.EchoReasoning
// (and the neighbors the same slip could take out) into the Def.
func TestReasoningEchoConfigWiring(t *testing.T) {
	st := newOpencodeEchoStub()
	defer st.srv.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Providers = append(cfg.Providers, config.ProviderCfg{
		Name: "w", Kind: "openai", BaseURL: st.srv.URL, APIKey: "up-key",
		Models:         []string{"deepseek-v4.1-flash"},
		AlwaysThinking: []string{"glm-*"},
		NoThinking:     []string{"kilo-auto/*"},
		EchoReasoning:  []string{"deepseek-*"},
	})
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	d, ok := srv.cur().pool.Get("w")
	if !ok {
		t.Fatal("provider w missing from pool")
	}
	if !d.ReasoningEchoModel("deepseek-v4.1-flash") {
		t.Fatalf("EchoReasoning glob not wired: %+v", d.EchoReasoning)
	}
	if !d.AlwaysThinkingModel("glm-5.3") {
		t.Fatalf("AlwaysThinking glob not wired: %+v", d.AlwaysThinking)
	}
	if !d.NoThinkingModel("kilo-auto/free") {
		t.Fatalf("NoThinking glob not wired: %+v", d.NoThinking)
	}
}

// TestReasoningEchoTrailingUserUntouched pins the cache-stability carve-out:
// a trailing user turn disables the upstream's echo validation, so the
// synthesis must leave the body untouched (no placeholder noise on the
// common turn shape).
func TestReasoningEchoTrailingUserUntouched(t *testing.T) {
	st := newOpencodeEchoStub()
	defer st.srv.Close()

	body, _ := json.Marshal(map[string]any{
		"model": "oc/deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{
				"id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "content": "out", "tool_call_id": "c1"},
			map[string]any{"role": "user", "content": "go on"},
		},
	})
	def := &provider.Def{Name: "oc", EchoReasoning: []string{"*"}}
	out := synthesizeReasoningEcho(body, "deepseek-v4.1-flash", def)
	if string(out) != string(body) {
		t.Fatalf("trailing-user body must pass through byte-identical:\n got %s\nwant %s", out, body)
	}
}
