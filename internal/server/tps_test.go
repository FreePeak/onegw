package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression (2026-09-11): the #19 ring's tok/s must be sane or absent —
// sub-floor windows (a buffered non-stream reply lands in <1ms) used to
// record 1.3M tok/s, which is not a decode speed. A real stream window
// must record a plausible one.
func TestRingTPSWindowFloor(t *testing.T) {
	// Slow same-format stream: 40 tokens over ~600ms (8 chunks × 75ms).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 8 {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(75 * time.Millisecond)
		}
		// One terminal usage chunk with the real total, like live upstreams.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":40}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer up.Close()

	cfg := makeCfg(t, "key-tps", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	req := chatReqStr(t, "p1/m1", "go", true)
	req.Header.Set("Authorization", "Bearer key-tps")
	w := do(t, h, req)
	if w.Code != 200 {
		t.Fatalf("stream request: got %d", w.Code)
	}

	entries := srv.reqlog.latest(10)
	if len(entries) == 0 {
		t.Fatal("no log entries recorded")
	}
	var e *logEntry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Code == 200 && entries[i].Provider == "p1" {
			e = &entries[i]
			break
		}
	}
	if e == nil {
		t.Fatal("no p1 200 row in ring")
	}
	// Sniffed usage: 40 output tokens over >=400ms of chunks => tps in
	// (0, 100]. Before the floor the window could read ~0 → huge tps.
	if e.Out != 40 {
		t.Fatalf("sniffed out tokens: got %d, want 40", e.Out)
	}
	if e.Tps <= 0 || e.Tps > 100 {
		t.Fatalf("ring tok/s must be plausible: got %f (ms=%d, out=%d)", e.Tps, e.Ms, e.Out)
	}
	if e.Ms < 200 {
		t.Fatalf("decode window must be carried: got %d ms", e.Ms)
	}

	// The EWMA respected its own floor all along: the provider reports
	// roughly the same speed.
	def, ok := srv.cur().pool.Get("p1")
	if !ok {
		t.Fatal("p1 missing from pool")
	}
	if tps := def.ProviderTPS(); tps <= 0 || tps > 100 {
		t.Fatalf("provider EWMA must be plausible too: %f", tps)
	}
}

// The EWMA itself must ignore the sub-floor window (provider gate), while
// the ring's floor keeps 0 — a fast non-stream reply records no speed.
func TestRingTPSAbsentForInstantReply(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":200}}`))
	}))
	defer up.Close()
	cfg := makeCfg(t, "key-tps2", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	req := chatReq(t, "p1/m1") // non-streaming client
	req.Header.Set("Authorization", "Bearer key-tps2")
	if w := do(t, srv.Handler(), req); w.Code != 200 {
		t.Fatalf("request: got %d", w.Code)
	}
	entries := srv.reqlog.latest(5)
	e := entries[len(entries)-1]
	if e.Tps != 0 || e.Ms != 0 {
		t.Fatalf("sub-floor windows must not record speed: ms=%d tps=%f", e.Ms, e.Tps)
	}
	def, _ := srv.cur().pool.Get("p1")
	if got := def.ProviderTPS(); got != 0 {
		t.Fatalf("EWMA must ignore sub-floor windows: %f", got)
	}
}
