package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Fast-path transient-failure retry (live incident 2026-09-09 10:52): the
// streaming single-shot path surfaced raw upstream 429/401s to clients
// even when the whole request body was still buffered and a replay via
// the buffered pipeline (retry + combo fall-through) was possible.
// ---------------------------------------------------------------------------

// flaky429Upstream 429s the first hit (shared concurrency-limit body),
// then serves a normal completion. Counts hits.
func flaky429Upstream() (*httptest.Server, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"The request rate exceeds the current model Concurrency limit 1200. Please reduce the request frequency.","type":"upstream_error","code":1302}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","model":"m1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong after retry"}}]}`))
	}))
	return srv, &hits
}

func TestStreamFastPathReplaysWholeBodyOnTransient429(t *testing.T) {
	up, hits := flaky429Upstream()
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "b-ai", up: up.URL, model: "m1"}))

	// Small body (fits the 16K peek window) => whole=true, replay possible.
	r := chatReq(t, "b-ai/m1")
	w := do(t, h, authReq(r))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong after retry") {
		t.Fatalf("replay request failed: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("upstream hits=%d, want 2 (one 429 + successful replay)", got)
	}
}

// Body whose model field sits BEYOND the 16K peek window never reaches
// the fast path: the buffered pipeline handles it, and its Execute retry
// (2 attempts, 1s+2s shared-concurrency backoff) must surface the final
// 429 with the shared-window Retry-After hint.
func TestBufferedPathSharedConcurrency429RetriesThenHints(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"The request rate exceeds the current model Concurrency limit 1200.","type":"upstream_error","code":1302}}`))
	}))
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "b-ai", up: up.URL, model: "m1"}))

	in := []byte(`{"filler":"` + strings.Repeat("y", 17<<10) + `","model":"b-ai/m1","messages":[{"role":"user","content":"ping"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(in))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, authReq(r))
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits=%d, want 2 (MaxAttempts with shared backoff)", got)
	}
	if w.Code != 429 {
		t.Fatalf("code=%d, want 429; body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "2" {
		t.Fatalf("Retry-After=%q, want 2 (shared window hint)", ra)
	}
	if !strings.Contains(w.Body.String(), "Concurrency limit") {
		t.Fatalf("upstream diagnostic must be preserved, got %s", w.Body.String())
	}
}

// Large body whose model IS within the peek window: the fast path takes
// it, Do consumes part of the live stream, replay is impossible — the
// answer must be the managed retryable 429 (Retry-After present, upstream
// diagnostic kept, exactly ONE upstream hit: no truncated replay).
func TestStreamFastPathLargeBodyAnswersManagedRetryable(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"The request rate exceeds the current model Concurrency limit 1200.","type":"upstream_error","code":1302}}`))
	}))
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "b-ai", up: up.URL, model: "m1"}))

	// "stream":true before the oversized messages value: scanTopLevel
	// sees the stream literal definitively, so the truncated prefix still
	// hands the request to the fast path with whole=false.
	in := []byte(`{"model":"b-ai/m1","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("y", 17<<10) + `"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(in))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, authReq(r))
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits=%d, want 1 (no truncated replay)", got)
	}
	if w.Code != 429 {
		t.Fatalf("code=%d, want 429; body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("managed retryable answer must carry Retry-After")
	}
	if !strings.Contains(w.Body.String(), "Concurrency limit") {
		t.Fatalf("upstream diagnostic must be preserved, got %s", w.Body.String())
	}
}

// Non-retryable failures (400 invalid request) must keep failing fast —
// no replay, no Retry-After.
func TestStreamFastPathBadRequestFailsFast(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"messages too long","type":"invalid_request_error"}}`))
	}))
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	r := chatReq(t, "p1/m1")
	w := do(t, h, authReq(r))
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits=%d, want 1", got)
	}
	if w.Code != 400 {
		t.Fatalf("code=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("non-retryable error must not carry Retry-After, got %q", ra)
	}
}
