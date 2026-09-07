package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"onegw/internal/config"
)

// capUpstream records what the fake upstream received for one request.
type capUpstream struct {
	mu      sync.Mutex
	body    []byte
	chunked bool
	clost   int64 // r.ContentLength as seen upstream
}

func (c *capUpstream) snapshot() (body []byte, chunked bool, clost int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.body...), c.chunked, c.clost
}

// captureStub is an upstream that records the incoming request and answers
// an OpenAI chat completion echoing the model it received.
func captureStub() (*httptest.Server, *capUpstream) {
	cap := &capUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.body = b
		cap.chunked = len(r.TransferEncoding) > 0
		cap.clost = r.ContentLength
		cap.mu.Unlock()

		var req struct {
			Model string `json:"model"`
		}
		model := "?"
		if json.Unmarshal(b, &req) == nil && req.Model != "" {
			model = req.Model
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "cmpl-test",
			"object":  "chat.completion",
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong from " + model}}},
		})
	}))
	return srv, cap
}

// streamCfg builds a config like makeCfg but with the streaming flag on and
// extra tuning applied via mut.
func streamCfg(t *testing.T, saver bool, mut func(*config.Config), provs ...providerSpec) *config.Config {
	t.Helper()
	cfg := makeCfg(t, "sk-test-key", "", saver, provs...)
	cfg.Server.StreamRequests = true
	if mut != nil {
		mut(cfg)
	}
	return cfg
}

func newStreamServer(t *testing.T, cfg *config.Config) (*Server, http.Handler) {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, srv.Handler()
}

func authReq(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer sk-test-key")
	return r
}

func TestStreamRelayByteFaithful(t *testing.T) {
	up, cap := captureStub()
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	// Odd key order, whitespace, and model last: the relay must preserve
	// every byte except the spliced model value.
	in := []byte(`{"temperature": 0.7,
  "messages": [{"role": "user", "content": "ping"}],
  "model": "p1/m1"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(in))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, authReq(r))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("relay request failed: code=%d body=%s", w.Code, w.Body.String())
	}
	body, _, _ := cap.snapshot()
	want := bytes.Replace(in, []byte(`"p1/m1"`), []byte(`"m1"`), 1)
	if !bytes.Equal(body, want) {
		t.Fatalf("upstream body not byte-faithful:\n got: %q\nwant: %q", body, want)
	}
}

func TestStreamRelayUpstreamSeesChunked(t *testing.T) {
	up, cap := captureStub()
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	w := do(t, h, authReq(chatReq(t, "p1/m1")))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	_, chunked, clost := cap.snapshot()
	if !chunked || clost != -1 {
		t.Fatalf("upstream saw Content-Length request: chunked=%v clen=%d (streaming must never fabricate a length)", chunked, clost)
	}
}

func TestStreamRelayLargeBodyFixedReservation(t *testing.T) {
	up, _ := captureStub()
	defer up.Close()
	cfg := streamCfg(t, false, func(c *config.Config) { c.Server.BufferCap = 1 << 20 },
		providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, h := newStreamServer(t, cfg)

	const size = 8 << 20
	// "stream":true must sit inside the peek window: a stream literal the
	// scan never saw is treated as UNKNOWN (buffer-pinned SSE clients may
	// hide it behind a >16 KiB messages array), forcing the buffered path.
	big := []byte(`{"model":"p1/m1","stream":true,"messages":["` + strings.Repeat("x", size) + `"]}`)

	stop := make(chan struct{})
	var mu sync.Mutex
	var maxHeld int64
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			held, _ := srv.cur().budget.Stats()
			mu.Lock()
			if held > maxHeld {
				maxHeld = held
			}
			mu.Unlock()
		}
	}()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(big))
			r.Header.Set("Content-Type", "application/json")
			w := do(t, h, authReq(r))
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	close(stop)
	mu.Lock()
	defer mu.Unlock()
	if maxHeld == 0 {
		t.Fatal("streaming path never took its budget reservation")
	}
	if maxHeld > 2*streamReserveBytes {
		t.Fatalf("held %d bytes during relay; expected the fixed %d reservation per stream", maxHeld, streamReserveBytes)
	}
}

func TestStreamRelayFallbacksKeepBufferedPath(t *testing.T) {
	up, cap := captureStub()
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	t.Run("model beyond peek window", func(t *testing.T) {
		cap.mu.Lock()
		cap.body = nil
		cap.mu.Unlock()
		in := []byte(`{"filler":"` + strings.Repeat("y", 17<<10) + `","model":"p1/m1","messages":[{"role":"user","content":"ping"}]}`)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(in))
		r.Header.Set("Content-Type", "application/json")
		w := do(t, h, authReq(r))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
			t.Fatalf("fallback request failed: code=%d body=%s", w.Code, w.Body.String())
		}
		body, chunked, _ := cap.snapshot()
		if chunked {
			t.Fatal("fallback must go out buffered (Content-Length set)")
		}
		if !strings.Contains(string(body), `"model":"m1"`) {
			t.Fatalf("buffered path did not rewrite model: %q", body)
		}
	})

	t.Run("developer role normalizes via buffered path", func(t *testing.T) {
		cap.mu.Lock()
		cap.body = nil
		cap.mu.Unlock()
		in := []byte(`{"model":"p1/m1","messages":[{"role":"developer","content":"sys"}]}`)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(in))
		r.Header.Set("Content-Type", "application/json")
		w := do(t, h, authReq(r))
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		body, chunked, _ := cap.snapshot()
		if chunked {
			t.Fatal("normalizeRoles-eligible body must stay buffered")
		}
		if !strings.Contains(string(body), `"role":"system"`) || strings.Contains(string(body), `"role":"developer"`) {
			t.Fatalf("developer role not normalized: %q", body)
		}
	})

	t.Run("non-JSON body errors like the buffered path", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte("not json")))
		r.Header.Set("Content-Type", "application/json")
		w := do(t, h, authReq(r))
		if w.Code != 400 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
	})
}

func TestStreamRelayBufferedPathStillRemarshals(t *testing.T) {
	// Flag off: same shape as the byte-faithful test, but the buffered path
	// re-marshals the map, so key order is NOT preserved.
	up, cap := captureStub()
	defer up.Close()
	cfg := makeCfg(t, "sk-test-key", "", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	_, h := newStreamServer(t, cfg)

	in := []byte(`{"temperature": 0.7, "model": "p1/m1", "messages": [{"role": "user", "content": "ping"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(in))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, authReq(r))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body, _, _ := cap.snapshot()
	if bytes.Equal(body, in) || strings.Contains(string(body), `"temperature": 0.7`) {
		t.Fatalf("flag-off path unexpectedly preserved formatting: %q", body)
	}
	if !strings.Contains(string(body), `"model":"m1"`) {
		t.Fatalf("model not rewritten: %q", body)
	}
}

func TestStreamRelayCrossFormatStaysBuffered(t *testing.T) {
	// Anthropic client vs openai provider: translation needs the full body,
	// so the streaming gate must not engage.
	up, cap := captureStub()
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	in := []byte(`{"model":"p1/m1","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(in))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", "sk-test-key")
	r.Header.Set("anthropic-version", "2023-06-01")
	w := do(t, h, r)
	// Non-streaming cross-format requests translate the response: the
	// anthropic client must receive an anthropic-shaped message object.
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"message"`) || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("expected translated anthropic response: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "chat.completion") {
		t.Fatalf("response not translated to anthropic shape: %s", w.Body.String())
	}
	body, chunked, _ := cap.snapshot()
	if chunked {
		t.Fatal("cross-format body must stay buffered")
	}
	// Translated to the provider's format: no anthropic-only max_tokens.
	if strings.Contains(string(body), "max_tokens") {
		t.Fatalf("upstream got untranslated anthropic body: %q", body)
	}
}

func TestStreamRelayAlwaysThinkingStaysBuffered(t *testing.T) {
	up, cap := captureStub()
	defer up.Close()
	cfg := streamCfg(t, false, func(c *config.Config) { c.Providers[0].AlwaysThinking = []string{"m*"} },
		providerSpec{name: "p1", up: up.URL, model: "m1"})
	_, h := newStreamServer(t, cfg)

	w := do(t, h, authReq(chatReq(t, "p1/m1")))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	_, chunked, _ := cap.snapshot()
	if chunked {
		t.Fatal("always-thinking model must stay buffered for knob adaptation")
	}
}

func TestStreamRelaySaverStaysBuffered(t *testing.T) {
	up, cap := captureStub()
	defer up.Close()
	_, h := newStreamServer(t, streamCfg(t, true, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	w := do(t, h, authReq(chatReq(t, "p1/m1")))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	_, chunked, _ := cap.snapshot()
	if chunked {
		t.Fatal("saver-enabled gateway must keep buffering bodies")
	}
}

func TestStreamRelayComboStaysBuffered(t *testing.T) {
	up1, cap1 := captureStub()
	defer up1.Close()
	up2, _ := captureStub()
	defer up2.Close()
	_, h := newStreamServer(t, streamCfg(t, false, nil,
		providerSpec{name: "p1", up: up1.URL, model: "m1"},
		providerSpec{name: "p2", up: up2.URL, model: "m2"}))

	w := do(t, h, authReq(chatReq(t, "pair")))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("combo request failed: code=%d body=%s", w.Code, w.Body.String())
	}
	_, chunked, _ := cap1.snapshot()
	if chunked {
		t.Fatal("combo targets must stay buffered to preserve fallback replay")
	}
}

func TestStreamRelaySaturationBlocksThenProceeds(t *testing.T) {
	up, _ := captureStub()
	defer up.Close()
	cfg := streamCfg(t, false, func(c *config.Config) { c.Server.BufferCap = 512 << 10 },
		providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, h := newStreamServer(t, cfg)

	// Hold the entire budget: the streaming reservation cannot be taken.
	b := srv.cur().budget
	if err := b.Acquire(context.Background(), 512<<10); err != nil {
		t.Fatalf("test hold: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		w := do(t, h, authReq(chatReq(t, "p1/m1")))
		done <- w.Code
	}()

	deadline := time.Now().Add(5 * time.Second)
	for b.Waiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("streaming request never blocked on the saturated budget")
		}
		time.Sleep(2 * time.Millisecond)
	}
	b.Release(512 << 10)

	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("request after release: code=%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never proceeded after budget release")
	}
}

func TestStreamRelaySaturatedCanceledRequest503(t *testing.T) {
	up, _ := captureStub()
	defer up.Close()
	cfg := streamCfg(t, false, func(c *config.Config) { c.Server.BufferCap = 512 << 10 },
		providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, h := newStreamServer(t, cfg)

	b := srv.cur().budget
	if err := b.Acquire(context.Background(), 512<<10); err != nil {
		t.Fatalf("test hold: %v", err)
	}
	defer b.Release(512 << 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := chatReq(t, "p1/m1").WithContext(ctx)
	w := do(t, h, authReq(r))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled request while saturated: code=%d body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("503 must carry Retry-After")
	}
}

func TestStreamRelayTooLarge413(t *testing.T) {
	up, _ := captureStub()
	defer up.Close()
	cfg := streamCfg(t, false, func(c *config.Config) { c.Server.MaxBody = 1024 },
		providerSpec{name: "p1", up: up.URL, model: "m1"})
	_, h := newStreamServer(t, cfg)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(make([]byte, 4096)))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, authReq(r))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized declared body: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStreamConfigDefaultsOff(t *testing.T) {
	cfg := &config.Config{}
	cfg.Defaults()
	if cfg.Server.StreamRequests {
		t.Fatal("stream_requests must default to false")
	}
}
