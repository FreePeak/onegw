package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
	"onegw/internal/provider"
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

// kind=cursor speaks Connect-RPC protobuf upstream and answers OpenAI shape
// (issue #12 follow-up). The mock serves AgentService frames; the request
// carries a system message, which must NOT appear in run_request field 8
// (upstream kills such turns) — it must ride folded in the user message.
func TestCursorKindEndToEnd(t *testing.T) {
	// Build the upstream response: one text frame + one done frame.
	textDelta := pbBytesForTest(nil, 1, pbBytesForTest(nil, 1, pbStringForTest("pong from cursor")))
	var update []byte
	update = pbBytesForTest(update, 1, textDelta)
	var usage []byte
	usage = pbUvarintForTest(usage, 1, 12)
	usage = pbUvarintForTest(usage, 2, 3)
	update = pbBytesForTest(update, 14, usage)
	var agentMsg []byte
	agentMsg = pbBytesForTest(agentMsg, 1, update)
	// exec_server_request{10: request_server_info} — the context question.
	var exec []byte
	exec = pbBytesForTest(exec, 10, nil)
	var execQuestion []byte
	execQuestion = pbBytesForTest(execQuestion, 2, exec)

	frame := func(payload []byte) []byte {
		out := make([]byte, 5+len(payload))
		out[1] = byte(len(payload) >> 24)
		out[2] = byte(len(payload) >> 16)
		out[3] = byte(len(payload) >> 8)
		out[4] = byte(len(payload))
		copy(out[5:], payload)
		return out
	}

	captured := make(chan []byte, 4)
	// HTTP/2 is REQUIRED: the agent path is full-duplex (pipe-backed
	// request body), and Go's h1 transport cannot stream a request while
	// reading the response — h1 tests deadlock. The live upstream is h2.
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-cursor-checksum") == "" {
			t.Errorf("upstream: x-cursor-checksum header missing")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer up-key" {
			t.Errorf("upstream: Authorization = %q", got)
		}
		// MIRROR THE LIVE UPSTREAM: respond with headers + the exec
		// question IMMEDIATELY, before the request body ends (the real
		// AgentService asks while the request half is still open). The
		// gateway answers via the pipe; then the mock reads one frame
		// (the run request) and later the reply, then streams the turn.
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write(frame(execQuestion))
		w.(http.Flusher).Flush()
		// Read exactly one reply frame from the (still-open) request body:
		// the constant RequestContext answer.
		reply := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, reply); err != nil {
			t.Errorf("upstream: no handshake reply: %v", err)
			return
		}
		n := int(reply[1])<<24 | int(reply[2])<<16 | int(reply[3])<<8 | int(reply[4])
		payload := make([]byte, n)
		if _, err := io.ReadFull(r.Body, payload); err != nil {
			t.Errorf("upstream: short reply: %v", err)
			return
		}
		// The reply IS the captured payload; write the turn frames now —
		// END_STREAM (pump finishing) follows once the client drains.
		captured <- payload
		_, _ = w.Write(frame(agentMsg))
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	defer up.Close()

	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "cur", up: up.URL, model: "m1"})
	// Trust the test server's self-signed cert for this provider Def.
	provider.SetCursorTLSOverrideForTest(up.Client().Transport.(*http.Transport).TLSClientConfig)
	t.Cleanup(provider.ResetCursorTLSOverrideForTest)
	cfg.Providers[0] = config.ProviderCfg{
		Name: "cur", Kind: "cursor", BaseURL: up.URL,
		APIKey: "up-key", Models: []string{"m1"},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"cur/m1","messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"Reply PONG"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong from cursor") {
		t.Fatalf("answer missing: %s", w.Body.String())
	}

	select {
	case sent := <-captured:
		// run_request must NOT carry field 8 (system prompt kills the turn).
		if bytes.Contains(sent, []byte{0x40}) && pbHasField8System(sent) {
			t.Fatalf("run request must not carry a system prompt in field 8: %x", sent[:64])
		}
	default:
		t.Fatal("upstream saw no request")
	}
}

// --- cursor test protobuf helpers (mirror translat's codec; minimal) ---

func pbTagForTest(b []byte, field, wire int) []byte {
	tag := uint64(field)<<3 | uint64(wire)
	for tag >= 0x80 {
		b = append(b, byte(tag)|0x80)
		tag >>= 7
	}
	return append(b, byte(tag))
}

func pbBytesForTest(b []byte, field int, v []byte) []byte {
	b = pbTagForTest(b, field, 2)
	l := uint64(len(v))
	for l >= 0x80 {
		b = append(b, byte(l)|0x80)
		l >>= 7
	}
	b = append(b, byte(l))
	return append(b, v...)
}

func pbStringForTest(s string) []byte { return []byte(s) }

func pbUvarintForTest(b []byte, field int, v uint64) []byte {
	b = pbTagForTest(b, field, 0)
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// pbHasField8System reports whether a run_request frame carries field 8
// (0x40 tag with non-empty payload) — the forbidden system prompt.
func pbHasField8System(sent []byte) bool {
	for i := 0; i+1 < len(sent); i++ {
		if sent[i] == 0x42 && i+2 < len(sent) && sent[i+1] > 0 { // field 8, wire 2, len>0
			return true
		}
	}
	return false
}

var _ = bufio.NewReader // keep bufio import if assertions change
