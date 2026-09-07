package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/config"
)

// recordingStub is upstreamStub plus a capture of the body the gateway
// actually forwarded, so tests can assert on injection/compression effects.
func recordingStub(t *testing.T, model string) (*httptest.Server, *[]byte) {
	t.Helper()
	seen := new([]byte)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*seen = b
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-test", "object": "chat.completion", "model": model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong from " + model}}},
		})
	}))
	return up, seen
}

func openAIReq(t *testing.T, key, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

const marker = "onegw-terse-directive"

func TestInjectE2EPrependsSystemDirective(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "terse", Models: []string{"m*"}}}
	cfg.Defaults()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if w := do(t, srv.Handler(), openAIReq(t, "k-test", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var fwd struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*seen, &fwd); err != nil {
		t.Fatalf("upstream body invalid: %v (%.200s)", err, *seen)
	}
	if len(fwd.Messages) != 2 || fwd.Messages[0].Role != "system" || !strings.Contains(fwd.Messages[0].Content, marker) {
		t.Fatalf("directive not injected upstream: %+v", fwd.Messages)
	}
	if fwd.Messages[1].Role != "user" || fwd.Messages[1].Content != "ping" {
		t.Fatalf("client message altered: %+v", fwd.Messages[1])
	}
	if fwd.Model != "m1" {
		t.Fatalf("model rewrite lost: %q", fwd.Model)
	}
}

func TestInjectE2EGlobMissAndExistingSystem(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "terse", Models: []string{"nope-*"}}}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	if w := do(t, srv.Handler(), openAIReq(t, "k-test", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if strings.Contains(string(*seen), marker) {
		t.Fatalf("injected despite glob miss")
	}
}

func TestInjectE2EAnthropicSurface(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Providers[0].Kind = "anthropic"
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "caveman"}}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	body := `{"model":"m1","system":"client system","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer k-test")
	if w := do(t, srv.Handler(), r); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// Same-format anthropic→anthropic: directive merged into the system
	// string ahead of the client's own text.
	var probe struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal(*seen, &probe); err != nil {
		t.Fatalf("bad upstream body: %v (%.200s)", err, *seen)
	}
	if !strings.HasPrefix(probe.System, marker) || !strings.Contains(probe.System, "client system") {
		t.Fatalf("anthropic system merge wrong: %q", probe.System)
	}
}

func TestInjectE2ECustomMode(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "custom", Text: "Answer in at most five words."}}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	if w := do(t, srv.Handler(), openAIReq(t, "k-test", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	s := string(*seen)
	i := strings.Index(s, marker)
	if i < 0 || !strings.Contains(s, "Answer in at most five words.") {
		t.Fatalf("custom text or marker missing upstream: %.300s", s)
	}
	// The marker prefixes the custom text (idempotency contract).
	if !strings.HasPrefix(s[i:], marker+": Answer in at most five words.") {
		t.Fatalf("custom text not marker-prefixed: %.300s", s[i:])
	}
}

func TestExternalCompressE2ESuccess(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	var gotMessages []byte
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotMessages = req.Messages
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": json.RawMessage(`[{"role":"user","content":"compressed-summary"}]`)})
	}))
	defer comp.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.External = config.ExternalCfg{Enabled: true, URL: comp.URL + "/v1/compress", MinBytes: 1, TimeoutMS: 1000}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	big := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("long ", 8000) + `"}]}`
	if w := do(t, srv.Handler(), openAIReq(t, "k-test", big)); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(*seen), "compressed-summary") {
		t.Fatalf("upstream did not receive compressed messages: %.200s", *seen)
	}
	if !strings.Contains(string(gotMessages), "long") {
		t.Fatalf("compress service did not receive the original messages")
	}
	if strings.Contains(string(*seen), strings.Repeat("long ", 100)) {
		t.Fatalf("uncompressed content leaked upstream")
	}
}

func TestExternalCompressE2EFailOpen(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kaput", http.StatusBadGateway)
	}))
	defer comp.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.External = config.ExternalCfg{Enabled: true, URL: comp.URL, MinBytes: 1}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	big := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}]}`
	if w := do(t, srv.Handler(), openAIReq(t, "k-test", big)); w.Code != 200 {
		t.Fatalf("fail-open must still answer 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(*seen), strings.Repeat("x", 1000)) {
		t.Fatalf("fail-open must forward the original body upstream")
	}
}

func TestExternalCompressE2ETimeout(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	}))
	defer comp.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.External = config.ExternalCfg{Enabled: true, URL: comp.URL, MinBytes: 1, TimeoutMS: 50}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	big := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}]}`
	start := time.Now()
	if w := do(t, srv.Handler(), openAIReq(t, "k-test", big)); w.Code != 200 {
		t.Fatalf("timeout must fail open with 200, got %d", w.Code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout did not bound the pipeline: %v", time.Since(start))
	}
	if !strings.Contains(string(*seen), strings.Repeat("x", 1000)) {
		t.Fatalf("timeout must forward the original body")
	}
}

func TestExternalCompressE2EFailClosed(t *testing.T) {
	up, _ := recordingStub(t, "m1")
	defer up.Close()

	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kaput", http.StatusInternalServerError)
	}))
	defer comp.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	no := false
	cfg.Saver.External = config.ExternalCfg{Enabled: true, URL: comp.URL, MinBytes: 1, FailOpen: &no}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	big := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}]}`
	w := do(t, srv.Handler(), openAIReq(t, "k-test", big))
	if w.Code != 502 {
		t.Fatalf("fail_open=false must answer 502, got %d: %s", w.Code, w.Body.String())
	}
}

// Cross-format: OpenAI client → Anthropic upstream picks the injected
// system up through the unified model.
func TestInjectCrossFormatTranslation(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Providers[0].Kind = "anthropic"
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "terse"}}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	if w := do(t, srv.Handler(), openAIReq(t, "k-test", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var probe struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(*seen, &probe); err != nil {
		t.Fatalf("bad upstream body: %v (%.300s)", err, *seen)
	}
	if !strings.Contains(string(probe.System), marker) {
		t.Fatalf("system field missing directive: %s", probe.System)
	}
	for _, m := range probe.Messages {
		if m.Role == "system" {
			t.Fatalf("system role leaked into anthropic wire messages")
		}
	}
}

// The [saver] enabled flag is the master token-saving switch: with it off,
// neither injection nor the external hook may touch the request.
func TestOutputSaversInertWhenSaverDisabled(t *testing.T) {
	up, seen := recordingStub(t, "m1")
	defer up.Close()

	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("compress service must not be called when saver disabled")
	}))
	defer comp.Close()

	cfg := makeCfg(t, "k-test", "pw", false, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "terse"}}
	cfg.Saver.External = config.ExternalCfg{Enabled: true, URL: comp.URL, MinBytes: 1}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	big := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}]}`
	if w := do(t, srv.Handler(), openAIReq(t, "k-test", big)); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if strings.Contains(string(*seen), marker) {
		t.Fatalf("injection applied while saver disabled")
	}
}

// A retry after a transient upstream failure must not accumulate a second
// directive: the body is injected once, before resolve/execute.
func TestInjectIdempotentAcrossUpstreamRetries(t *testing.T) {
	attempts := 0
	seen := new([]byte)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*seen = b
		attempts++
		if attempts == 1 {
			http.Error(w, `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-test", "object": "chat.completion", "model": "m1",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong"}}},
		})
	}))
	defer up.Close()

	cfg := makeCfg(t, "k-test", "pw", true, providerSpec{name: "up", up: up.URL, model: "m1"})
	cfg.Saver.Inject = []config.InjectCfg{{Mode: "terse"}}
	cfg.Defaults()
	srv, _ := New(cfg)
	defer srv.Close()

	if w := do(t, srv.Handler(), openAIReq(t, "k-test", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if attempts < 2 {
		t.Fatalf("test needs a retry; attempts=%d", attempts)
	}
	if n := strings.Count(string(*seen), marker); n != 1 {
		t.Fatalf("directive appears %d times on the retried request", n)
	}
}
