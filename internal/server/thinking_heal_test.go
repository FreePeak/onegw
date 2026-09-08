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

// thinkingStub is a GLM-style always-thinking upstream: chat requests whose
// reasoning_effort is outside low|high|max are rejected with the signature
// 400 (B.AI shape: code 400001 + Chinese message); valid ones are served.
// It records the reasoning_effort of every chat request in arrival order.
type thinkingStub struct {
	mu      sync.Mutex
	srv     *httptest.Server
	efforts []string
	reject  bool // reject even valid efforts (for fall-through tests)
}

func newThinkingStub(reject bool) *thinkingStub {
	ts := &thinkingStub{reject: reject}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.Unmarshal(b, &req)
		ts.mu.Lock()
		ts.efforts = append(ts.efforts, req.ReasoningEffort)
		reject := ts.reject
		ts.mu.Unlock()
		switch req.ReasoningEffort {
		case "low", "high", "max":
			if reject {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"400001","type":"invalid_request_error","message":"The request is invalid: 该模型始终思考，不支持关闭思考；请使用 low、high 或 max。"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-ok", "object": "chat.completion", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "pong from " + req.Model}}},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"400001","type":"invalid_request_error","message":"The request is invalid: 该模型始终思考，不支持关闭思考；请使用 low、high 或 max。"}}`))
		}
	}))
	return ts
}

func (ts *thinkingStub) seen() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.efforts...)
}

func effortReq(t *testing.T, model, effort string, stream bool) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":            model,
		"messages":         []any{map[string]any{"role": "user", "content": "ping"}},
		"reasoning_effort": effort,
		"stream":           stream,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	return r
}

// TestAlwaysThinkingSelfHealRetry covers the incident: a combo "free"-style
// target (GLM, always thinking) with NO always_thinking globs configured,
// hit with an unrecognized effort (xhigh). The gateway must learn, retry
// the same target with the coerced effort, and never surface the 400.
func TestAlwaysThinkingSelfHealRetry(t *testing.T) {
	glm := newThinkingStub(false)
	defer glm.srv.Close()
	ok, _ := captureStub()
	defer ok.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "glm", up: glm.srv.URL, model: "glm-5.3-flash"},
		providerSpec{name: "deep", up: ok.URL, model: "deepseek-v4"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// First request: raw xhigh bounces once (learning), coerced retry passes.
	w := do(t, h, effortReq(t, "pair", "xhigh", false))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from glm-5.3-flash") {
		t.Fatalf("self-heal failed: code=%d body=%s", w.Code, w.Body.String())
	}
	got := glm.seen()
	if len(got) != 2 || got[0] != "xhigh" || got[1] != "max" {
		t.Fatalf("upstream saw efforts %v, want [xhigh max] (one bounce, coerced retry)", got)
	}

	// Second request: learned state coerces upfront — one upstream call.
	w = do(t, h, effortReq(t, "pair", "xhigh", false))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from glm-5.3-flash") {
		t.Fatalf("learned request failed: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := glm.seen(); len(got) != 3 || got[2] != "max" {
		t.Fatalf("after learning upstream saw efforts %v, want third = max with no bounce", got)
	}
}

// TestAlwaysThinkingFallthroughToNextTarget covers the combo case the user
// asked for: models with different thinking modes share a combo, the
// always-thinking one cannot serve the request even coerced, so the chain
// must fall through to the next target instead of surfacing the 400.
func TestAlwaysThinkingFallthroughToNextTarget(t *testing.T) {
	glm := newThinkingStub(true) // rejects everything with the signature
	defer glm.srv.Close()
	ok, deepCap := captureStub()
	defer ok.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "glm", up: glm.srv.URL, model: "glm-5.3-flash"},
		providerSpec{name: "deep", up: ok.URL, model: "deepseek-v4"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	w := do(t, h, effortReq(t, "pair", "xhigh", false))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from deepseek-v4") {
		t.Fatalf("fall-through failed: code=%d body=%s", w.Code, w.Body.String())
	}
	// GLM got MaxAttempts(2) calls — the retry coerced — then the chain
	// moved on; deepseek served the original body untouched.
	if got := glm.seen(); len(got) != 2 || got[0] != "xhigh" || got[1] != "max" {
		t.Fatalf("glm saw efforts %v, want [xhigh max]", got)
	}
	body, _, _ := deepCap.snapshot()
	if !strings.Contains(string(body), `"reasoning_effort":"xhigh"`) {
		t.Fatalf("next target must receive the uncoerced body, got %s", body)
	}
}

// TestStreamRelayLearnsAlwaysThinking covers the streaming fast path: the
// first request rides the single-shot relay, hits the always-thinking 400,
// and teaches the model; the next stream request then goes buffered (where
// the body is coerced) and succeeds.
func TestStreamRelayLearnsAlwaysThinking(t *testing.T) {
	glm := newThinkingStub(false)
	defer glm.srv.Close()
	cfg := streamCfg(t, false, nil, providerSpec{name: "glm", up: glm.srv.URL, model: "glm-5.3-flash"})
	_, h := newStreamServer(t, cfg)

	w := do(t, h, effortReq(t, "glm/glm-5.3-flash", "xhigh", true))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "始终思考") {
		t.Fatalf("single-shot fast path must surface the 400 once: code=%d body=%s", w.Code, w.Body.String())
	}

	// Learned: the model is now routed through the buffered pipeline.
	w = do(t, h, effortReq(t, "glm/glm-5.3-flash", "xhigh", true))
	if w.Code != 200 {
		t.Fatalf("learned stream request failed: code=%d body=%s", w.Code, w.Body.String())
	}
	got := glm.seen()
	if len(got) != 2 || got[0] != "xhigh" || got[1] != "max" {
		t.Fatalf("upstream saw efforts %v, want [xhigh max]", got)
	}
}
