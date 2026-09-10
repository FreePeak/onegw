package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"onegw/internal/config"
)

// idempotencyUpstream returns an openai stub that counts requests.
func idempotencyUpstream(t *testing.T, sse bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if sse {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)
			f.Flush()
			_, _ = w.Write([]byte("data: {\"id\":\"e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			f.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	return up, &calls
}

func idempotencyCfg(t *testing.T, up *httptest.Server) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-client"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "p1", Kind: "openai", BaseURL: up.URL, Models: []string{"m1"}, APIKey: "up-key",
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func idemChatReq(t *testing.T, idemKey, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-client")
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	return r
}

// Two identical POSTs with the same Idempotency-Key must hit the upstream
// exactly once; the retry replays the first response byte-for-byte.
func TestIdempotencyKeyDedupsUpstreamCall(t *testing.T) {
	up, calls := idempotencyUpstream(t, false)
	_, h := newTestServer2(t, idempotencyCfg(t, up))
	body := `{"model":"p1/m1","messages":[{"role":"user","content":"ping"}]}`

	w1 := do(t, h, idemChatReq(t, "op-123", body))
	w2 := do(t, h, idemChatReq(t, "op-123", body))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", w1.Code, w1.Body.String())
	}
	if w2.Code != w1.Code || w2.Body.String() != w1.Body.String() {
		t.Fatalf("replay differs: %d %q vs %d %q", w1.Code, w1.Body.String(), w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("second response must carry the replay marker")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1", n)
	}

	// Same key, different body: a distinct (key, body-hash) pair executes.
	w3 := do(t, h, idemChatReq(t, "op-123", `{"model":"p1/m1","messages":[{"role":"user","content":"ping2"}]}`))
	if w3.Code != http.StatusOK || strings.Contains(w3.Body.String(), "Replayed") {
		t.Fatalf("different body must execute: %d %s", w3.Code, w3.Body.String())
	}

	// No header: zero participation, upstream executes again.
	w4 := do(t, h, idemChatReq(t, "", body))
	if w4.Code != http.StatusOK {
		t.Fatalf("unkeyed request: %d %s", w4.Code, w4.Body.String())
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("upstream hit %d times total, want 3 (1 deduped pair + 2 executions)", n)
	}
}

// A stream-shaped original is never replayed: the retry gets 409.
func TestIdempotencyStreamReplayConflicts(t *testing.T) {
	up, calls := idempotencyUpstream(t, true)
	_, h := newTestServer2(t, idempotencyCfg(t, up))
	body := `{"model":"p1/m1","messages":[{"role":"user","content":"ping"}],"stream":true}`

	w1 := do(t, h, idemChatReq(t, "sse-1", body))
	if w1.Code != http.StatusOK || !strings.Contains(w1.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("first stream request: %d ct=%s", w1.Code, w1.Header().Get("Content-Type"))
	}
	w2 := do(t, h, idemChatReq(t, "sse-1", body))
	if w2.Code != http.StatusConflict {
		t.Fatalf("stream replay: %d %s, want 409", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "idempotency_conflict") {
		t.Fatalf("409 body must name the conflict: %s", w2.Body.String())
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1", n)
	}
}

// With the feature disabled ("off") the same retry executes upstream twice.
func TestIdempotencyOffExecutesTwice(t *testing.T) {
	up, calls := idempotencyUpstream(t, false)
	cfg := idempotencyCfg(t, up)
	cfg.Server.IdempotencyTTL = "off"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("off config invalid: %v", err)
	}
	_, h := newTestServer2(t, cfg)
	body := `{"model":"p1/m1","messages":[{"role":"user","content":"ping"}]}`
	w1 := do(t, h, idemChatReq(t, "op-123", body))
	w2 := do(t, h, idemChatReq(t, "op-123", body))
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("requests failed: %d %d", w1.Code, w2.Code)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("disabled feature must not dedup: upstream hit %d times, want 2", n)
	}
}
