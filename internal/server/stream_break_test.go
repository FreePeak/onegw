package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/config"
)

// sseStaller answers an OpenAI SSE stream with headers + one event, then
// goes silent forever (a half-open upstream socket after a laptop suspend,
// from the gateway's seat). Passing gap/n keeps emitting events every gap
// for n rounds instead of stalling immediately.
func sseStaller(t *testing.T, gap time.Duration, n int, stallAfter time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"e1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		f.Flush()
		for i := 0; i < n; i++ {
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
			f.Flush()
		}
		if stallAfter > 0 {
			// Clean end: gap + [DONE] + RETURN (handler exit ends the
			// stream, so the relay sees EOF, not a stall).
			select {
			case <-time.After(stallAfter):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			f.Flush()
			return
		}
		<-r.Context().Done() // stall forever: the breaker must kill us
	}))
}

func streamBreakCfg(t *testing.T, up *httptest.Server) *config.Config {
	t.Helper()
	cfg := makeCfg(t, "sk-test-key", "", false, providerSpec{"p1", up.URL, "m1"})
	cfg.Server.StreamRequests = true
	return cfg
}

func postStream(t *testing.T, url string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "p1/m1",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
		"stream":   true,
	})
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func withIdleLimit(t *testing.T, d time.Duration) {
	t.Helper()
	prev := upstreamBodyIdleLimit
	upstreamBodyIdleLimit = d
	t.Cleanup(func() { upstreamBodyIdleLimit = prev })
}

// The same-format passthrough must hand each upstream event to the client
// as it arrives (no 4KiB hold), and a stalled stream must be broken by the
// idle limit with an honest terminal error frame instead of hanging.
func TestRelayPerChunkFlushAndIdleBreak(t *testing.T) {
	withIdleLimit(t, 1200*time.Millisecond)
	up := sseStaller(t, 0, 0, 0)
	t.Cleanup(up.Close)
	_, h := newStreamServer(t, streamBreakCfg(t, up))
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	resp := postStream(t, gw.URL)
	defer resp.Body.Close()

	// Per-chunk flush: the first event must arrive well inside the idle
	// window — with the old 4KiB buffering nothing would reach us until
	// the breaker fired at 1200ms.
	goRead := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		goRead <- buf[:n]
	}()
	var head []byte
	select {
	case head = <-goRead:
		if len(head) == 0 {
			t.Fatal("stream ended before the first event")
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("first byte delayed: per-chunk flush regressed")
	}
	restCh := make(chan []byte, 1)
	go func() { rest, _ := io.ReadAll(resp.Body); restCh <- rest }()
	var rest []byte
	select {
	case rest = <-restCh:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled relay never terminated: idle breaker dead")
	}
	all := string(head) + string(rest)
	if !strings.Contains(all, "first") {
		t.Fatalf("stream lost the delivered event: %q", all)
	}
	if !strings.Contains(all, "upstream stream interrupted") || !strings.Contains(all, "[DONE]") {
		t.Fatalf("stalled relay must end with a terminal error frame + [DONE], got %q", all)
	}
}

// A stream that trickles inside the idle window must complete untouched:
// the breaker re-arms per read and never kills a live stream.
func TestIdleBreakSparesTricklingStream(t *testing.T) {
	withIdleLimit(t, 600*time.Millisecond)
	up := sseStaller(t, 100*time.Millisecond, 8, 200*time.Millisecond) // ~1s total, gaps << limit
	t.Cleanup(up.Close)
	_, h := newStreamServer(t, streamBreakCfg(t, up))
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	resp := postStream(t, gw.URL)
	defer resp.Body.Close()
	restCh := make(chan []byte, 1)
	go func() { all, _ := io.ReadAll(resp.Body); restCh <- all }()
	var all []byte
	select {
	case all = <-restCh:
	case <-time.After(5 * time.Second):
		t.Fatal("trickling stream never completed")
	}
	if strings.Contains(string(all), "upstream stream interrupted") {
		t.Fatalf("live stream was killed by the idle breaker: %q", all)
	}
	if !strings.Contains(string(all), "[DONE]") {
		t.Fatalf("healthy stream must relay intact through [DONE]: %q", all)
	}
}

// A saturated budget must answer fast, not park the request behind a
// hung stream's reservation (the post-wake retry queueing bug).
func TestBudgetWaitCapFailsFast(t *testing.T) {
	prev := budgetWaitLimit
	budgetWaitLimit = 50 * time.Millisecond
	t.Cleanup(func() { budgetWaitLimit = prev })

	b := NewByteBudget(1024)
	if err := b.Acquire(context.Background(), 1024); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Acquire(context.Background(), 512) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("full budget must fail once the wait cap elapses")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire waited unbounded behind a full budget")
	}
	b.Release(1024)
}

// The buffered cross-format path (non-stream client, different upstream
// format — e.g. an Anthropic client on an OpenAI provider) reads the whole
// body with no other timer. A half-open post-suspend body there pins the
// request budget until the idle breaker reaps it; assert the reaping works.
func TestBufferedCrossFormatIdleBreak(t *testing.T) {
	withIdleLimit(t, 400*time.Millisecond)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":`) // a first byte: headers+body begun, then silence
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // never finishes
	}))
	t.Cleanup(up.Close)
	cfg := makeCfg(t, "sk-test-key", "", false, providerSpec{"p1", up.URL, "m1"})
	_, h := newTestServer2(t, cfg) // StreamRequests off: buffered pipeline

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p1/m1","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(t, h, req) }()
	select {
	case w := <-done:
		// 502 (not a 5s hang, not a 200 with a truncated body): the
		// idle breaker reaped the stalled read and the buffered path
		// answered its usual upstream_read_failed — the exact type
		// string is normalized by the client-format error encoder.
		if w.Code != http.StatusBadGateway {
			t.Fatalf("stalled buffered relay must answer 502, got %d %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("buffered cross-format relay never reaped the stalled body")
	}
}
