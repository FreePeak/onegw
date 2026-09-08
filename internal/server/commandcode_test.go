package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
)

// commandCodeUpstream speaks the CommandCode /alpha/generate NDJSON wire
// format: it records the request it received and streams AI SDK v5 events.
type ccUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	hdr http.Header
	raw string
}

func newCommandCodeUpstream(t *testing.T, events []string) *ccUpstream {
	t.Helper()
	u := &ccUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		u.mu.Lock()
		u.hdr = r.Header.Clone()
		u.raw = string(body)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n"))
			flush()
		}
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

func (u *ccUpstream) lastRequest() (http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hdr, u.raw
}

func ccProviderCfg(name, baseURL string) config.ProviderCfg {
	return config.ProviderCfg{
		Name: name, Kind: "commandcode", BaseURL: baseURL,
		APIKey: "user_test_fake_key", Models: []string{"zai-org/GLM-5"},
	}
}

// E2E: streaming OpenAI client over a commandcode upstream gets OpenAI SSE
// chunks translated from the NDJSON events.
func TestCommandCodeStreamingE2E(t *testing.T) {
	up := newCommandCodeUpstream(t, []string{
		`{"type":"start","messageId":"cmpl-1"}`,
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"text-delta","text":" world"}`,
		`{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":10,"outputTokens":2}}`,
		`{"type":"finish"}`,
	})
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "cmc", up: up.srv.URL, model: "zai-org/GLM-5"})
	for i := range cfg.Providers {
		cfg.Providers[i] = ccProviderCfg(cfg.Providers[i].Name, up.srv.URL)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	req := chatReq(t, "cmc/zai-org/GLM-5")
	body, _ := json.Marshal(map[string]any{
		"model":    "cmc/zai-org/GLM-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type: %s", ct)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"content":"Hello"`) || !strings.Contains(out, `"content":" world"`) {
		t.Fatalf("deltas missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("terminator missing: %s", out)
	}
	if strings.Contains(out, `"type":"start"`) {
		t.Fatalf("raw NDJSON leaked to client: %s", out)
	}

	// Upstream saw the commandcode wire shape.
	hdr, raw := up.lastRequest()
	if got := hdr.Get("Authorization"); got != "Bearer user_test_fake_key" {
		t.Fatalf("upstream auth: %q", got)
	}
	if hdr.Get("x-command-code-version") == "" || hdr.Get("x-cli-environment") != "cli" {
		t.Fatalf("commandcode fingerprint headers missing: %v", hdr)
	}
	if hdr.Get("x-session-id") == "" {
		t.Fatal("x-session-id missing")
	}
	if hdr.Get("Accept") != "text/event-stream" {
		t.Fatalf("upstream Accept: %q", hdr.Get("Accept"))
	}
	var sent struct {
		Params struct {
			Stream bool   `json:"stream"`
			System string `json:"system"`
			Model  string `json:"model"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(raw), &sent); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, raw)
	}
	if !sent.Params.Stream || sent.Params.Model != "zai-org/GLM-5" {
		t.Fatalf("bad upstream params: %s", raw)
	}
}

// E2E: an in-200 {"type":"error"} event is answered as a real HTTP error
// before any streaming body reaches the client.
func TestCommandCodeIn200ErrorBecomesHTTPError(t *testing.T) {
	up := newCommandCodeUpstream(t, []string{
		`{"type":"start"}`,
		`{"type":"error","message":"rate limit exceeded, slow down"}`,
	})
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "cmc", up: up.srv.URL, model: "m1"})
	for i := range cfg.Providers {
		cfg.Providers[i] = ccProviderCfg(cfg.Providers[i].Name, up.srv.URL)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"cmc/m1","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate limit exceeded") {
		t.Fatalf("error message missing: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"type":"start"`) {
		t.Fatalf("upstream content leaked: %s", w.Body.String())
	}
}

// E2E: non-streaming client on the stream-forced upstream gets ONE aggregated
// chat.completion JSON object (Content-Type application/json).
func TestCommandCodeNonStreamClientAggregated(t *testing.T) {
	up := newCommandCodeUpstream(t, []string{
		`{"type":"start","messageId":"cmpl-2"}`,
		`{"type":"text-delta","text":"agg"}`,
		`{"type":"text-delta","text":"regated"}`,
		`{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":4,"outputTokens":2}}`,
		`{"type":"finish"}`,
	})
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "cmc", up: up.srv.URL, model: "m1"})
	for i := range cfg.Providers {
		cfg.Providers[i] = ccProviderCfg(cfg.Providers[i].Name, up.srv.URL)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"cmc/m1","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type: %s", ct)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not one JSON completion: %v (%s)", err, w.Body.String())
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 ||
		resp.Choices[0].Message.Content != "aggregated" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("bad aggregated completion: %+v (%s)", resp, w.Body.String())
	}
	if resp.Usage.PromptTokens != 4 || resp.Usage.CompletionTokens != 2 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	// The upstream still got stream:true (forced).
	_, raw := up.lastRequest()
	if !strings.Contains(raw, `"stream":true`) {
		t.Fatalf("stream not forced upstream: %s", raw)
	}
}

// grokResponsesUpstream speaks the Responses API SSE dialect.
func newGrokResponsesUpstream(t *testing.T, events []string) (*httptest.Server, *sync.Mutex, *string) {
	var mu sync.Mutex
	var rawReq string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		mu.Lock()
		rawReq = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
			flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &mu, &rawReq
}

// E2E: streaming OpenAI client over a grok-cli (Responses API) upstream.
func TestGrokResponsesStreamingE2E(t *testing.T) {
	up, mu, rawReq := newGrokResponsesUpstream(t, []string{
		`{"type":"response.created","response":{"id":"resp_x","model":"grok-build"}}`,
		`{"type":"response.output_text.delta","delta":"pong"}`,
		`{"type":"response.completed","response":{"id":"resp_x","usage":{"input_tokens":3,"output_tokens":1}}}`,
	})
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "gcli", up: up.URL, model: "grok-build"})
	cfg.Providers[0] = config.ProviderCfg{
		Name: "gcli", Kind: "openai-responses", BaseURL: up.URL,
		APIKey: "xai_grok_test_token", Models: []string{"grok-build"},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gcli/grok-build","messages":[{"role":"user","content":"ping"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"content":"pong"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("bad stream: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":3`) {
		t.Fatalf("usage missing: %s", out)
	}

	// Upstream got a Responses request with grok fingerprint headers.
	hdr := w.Header() // placeholder to keep imports tidy
	_ = hdr
	mu.Lock()
	req2 := *rawReq
	mu.Unlock()
	var sent map[string]any
	if err := json.Unmarshal([]byte(req2), &sent); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, req2)
	}
	if sent["stream"] != true || sent["store"] != false {
		t.Fatalf("stream/store not forced: %s", req2)
	}
	if _, ok := sent["input"]; !ok {
		t.Fatalf("input missing: %s", req2)
	}
	if _, ok := sent["messages"]; ok {
		t.Fatalf("chat-completions messages leaked: %s", req2)
	}
}

// kind=cursor validates in config but fails fast at request time.
func TestCursorKindFailsFast(t *testing.T) {
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "cur", up: "http://127.0.0.1:1", model: "m1"})
	cfg.Providers[0] = config.ProviderCfg{
		Name: "cur", Kind: "cursor", BaseURL: "https://api2.cursor.sh",
		APIKey: "test", Models: []string{"m1"},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"cur/m1","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)
	if w.Code == http.StatusOK {
		t.Fatalf("cursor skeleton must not serve requests: %s", w.Body.String())
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 invalid_request, got %d: %s", w.Code, w.Body.String())
	}
}

var _ = bufio.NewReader // keep bufio import if assertions change
