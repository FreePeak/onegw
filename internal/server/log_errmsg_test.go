package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The 2026-09-08 incident: a 90-minute 502 storm showed every dashboard row
// as bare "502 · upstream_error" with no explanation, because the request
// log discarded the upstream error message. The log must carry the upstream
// explanation so an operator can tell upstream-side failures from gateway
// bugs without re-running probes by hand.
func TestUpstreamErrorMessageReachesRequestLog(t *testing.T) {
	const upstreamBody = `{"error":{"message":"upstream is having a bad day","type":"api_error"}}`
	bad := metricsFailingStubMsg(502, upstreamBody)
	defer bad.Close()

	cfg := makeCfg(t, "key-log", "pw", false, providerSpec{name: "p1", up: bad.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer key-log")
	if w := do(t, h, r); w.Code != 502 {
		t.Fatalf("request: got %d, want 502, body %s", w.Code, w.Body.String())
	}

	entries := srv.reqlog.latest(10)
	if len(entries) == 0 {
		t.Fatal("no log entries recorded")
	}
	e := entries[len(entries)-1]
	// Kind is now the upstream's own error type (api_error from the stub
	// body), not the coarse bucket.
	if e.Kind != "api_error" || e.Code != 502 {
		t.Fatalf("entry: got %d/%s, want 502/api_error", e.Code, e.Kind)
	}
	if !strings.Contains(e.Err, "upstream is having a bad day") {
		t.Fatalf("log entry must carry the upstream message, got %q", e.Err)
	}

	// The JSON surface the console-log view consumes must include it too.
	w := httptest.NewRequest(http.MethodGet, "/admin/api/v1/logs?limit=10", nil)
	w.Header.Set("X-Admin-Password", "pw")
	res := do(t, h, w)
	if res.Code != 200 {
		t.Fatalf("logs API: got %d", res.Code)
	}
	var payload struct {
		Entries []logEntry `json:"entries"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("logs API json: %v", err)
	}
	if len(payload.Entries) == 0 {
		t.Fatal("logs API returned no entries")
	}
	last := payload.Entries[len(payload.Entries)-1]
	if !strings.Contains(last.Err, "upstream is having a bad day") {
		t.Fatalf("logs API must expose the upstream message, got %q", last.Err)
	}
}

// Long upstream bodies (up to 1 MiB are read) must be capped in the ring so
// one bad upstream cannot bloat log entries or SSE frames.
func TestUpstreamErrorMessageTruncated(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	bad := metricsFailingStubMsg(502, huge)
	defer bad.Close()

	cfg := makeCfg(t, "key-log2", "pw", false, providerSpec{name: "p1", up: bad.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer key-log2")
	if w := do(t, h, r); w.Code != 502 {
		t.Fatalf("request: got %d, want 502", w.Code)
	}

	entries := srv.reqlog.latest(10)
	if len(entries) == 0 {
		t.Fatal("no log entries recorded")
	}
	e := entries[len(entries)-1]
	if len(e.Err) > 400 {
		t.Fatalf("stored error must be capped, got %d bytes", len(e.Err))
	}
	if !strings.HasSuffix(e.Err, "…") {
		t.Fatalf("capped error should end with ellipsis, got %q", e.Err)
	}
}

// metricsFailingStubMsg answers with a JSON upstream error body carrying
// the given message, like a real provider 5xx.
func metricsFailingStubMsg(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// An unroutable model must log WHY (the router's "unknown provider x")
// alongside the fixed unresolved label — same diagnosability contract as
// upstream errors.
func TestNoRouteMessageReachesRequestLog(t *testing.T) {
	up := metricsUpstreamStub(t)
	defer up.Close()
	cfg := makeCfg(t, "key-nr", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	r := chatReq(t, "nosuch/m")
	r.Header.Set("Authorization", "Bearer key-nr")
	if w := do(t, h, r); w.Code != 404 {
		t.Fatalf("request: got %d, want 404", w.Code)
	}

	entries := srv.reqlog.latest(10)
	if len(entries) == 0 {
		t.Fatal("no log entries recorded")
	}
	e := entries[len(entries)-1]
	if e.Kind != "no_route" || e.Code != 404 {
		t.Fatalf("entry: got %d/%s, want 404/no_route", e.Code, e.Kind)
	}
	if !strings.Contains(e.Err, "unknown provider nosuch") {
		t.Fatalf("log entry must carry the router reason, got %q", e.Err)
	}
}

// A stream-only upstream that answers 200 and then fails in-stream with a
// one-api style 429 error object must (a) reach the client as 429, not
// 502, and (b) put the account into cooldown so the pool stops re-picking
// it — before 2026-09-08 the hardcoded in-stream 502 fed neither the
// cooldown ladder nor an honest log row.
func TestInStream429CoolsAccountAndLogs(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"type":"response.failed","response":{"error":{"code":"429","message":"rate limit exceeded, key xxx"}}}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer up.Close()

	// Stream-only upstream (forced-stream) + non-streaming client: the
	// aggregate path must classify the in-stream error and cool the pool.
	cfg := makeCfg(t, "key-s429", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	cfg.Providers[0].Kind = "openai-responses"
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer key-s429")
	if w := do(t, h, r); w.Code != 429 {
		t.Fatalf("in-stream 429 surfaced as %d, want 429 (body %s)", w.Code, w.Body.String())
	}

	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	entries := srv.reqlog.latest(10)
	if len(entries) == 0 {
		t.Fatal("no log entries recorded")
	}
	e := entries[len(entries)-1]
	if e.Code != 429 || e.Kind != "upstream_error" {
		t.Fatalf("entry: got %d/%s, want 429/upstream_error", e.Code, e.Kind)
	}
	if !strings.Contains(e.Err, "rate limit exceeded") {
		t.Fatalf("entry err must carry the in-stream message, got %q", e.Err)
	}
}
