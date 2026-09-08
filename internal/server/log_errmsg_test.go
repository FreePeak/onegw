package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if e.Kind != "upstream_error" || e.Code != 502 {
		t.Fatalf("entry: got %d/%s, want 502/upstream_error", e.Code, e.Kind)
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
