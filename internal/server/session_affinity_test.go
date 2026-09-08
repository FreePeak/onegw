package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureHeaderStub answers a completion and records one upstream header.
func captureHeaderStub(t *testing.T, header string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		seen = append(seen, r.Header.Get(header))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func authClientKey(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer sk-client")
	return r
}

// A client-sent x-grok-conv-id must reach the upstream verbatim through the
// buffered pipeline (issue #36: the live xai route loses it today because
// the gateway builds a fresh upstream request).
func TestClientConvIdReachesUpstreamBuffered(t *testing.T) {
	up, seen := captureHeaderStub(t, "X-Grok-Conv-Id")
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	_, h := newTestServer2(t, cfg)

	r := chatReq(t, "p1/m1")
	r.Header.Set("x-grok-conv-id", "conv-123")
	w := do(t, h, authClientKey(r))
	if w.Code != 200 {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	if len(*seen) != 1 || (*seen)[0] != "conv-123" {
		t.Fatalf("upstream saw %v, want [conv-123]", *seen)
	}

	// A request without the header must not invent one (knob off).
	if w := do(t, h, authClientKey(chatReq(t, "p1/m1"))); w.Code != 200 {
		t.Fatalf("second request failed: %d", w.Code)
	}
	if len(*seen) != 2 || (*seen)[1] != "" {
		t.Fatalf("ungated provider invented header: %v", *seen)
	}
}

// The stream fast path forwards the header too.
func TestClientConvIdReachesUpstreamStream(t *testing.T) {
	up, seen := captureHeaderStub(t, "X-Grok-Conv-Id")
	_, h := newStreamServer(t, streamCfg(t, false, nil, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	body := `{"model":"p1/m1","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	r.Header.Set("x-grok-conv-id", "conv-stream")
	w := do(t, h, r)
	if w.Code != 200 {
		t.Fatalf("stream request failed: %d %s", w.Code, w.Body.String())
	}
	if len(*seen) != 1 || (*seen)[0] != "conv-stream" {
		t.Fatalf("stream upstream saw %v, want [conv-stream]", *seen)
	}
}

// With the provider opted in via session_header, a request with no client
// header gets a stable per-key derived id upstream.
func TestDerivedSessionHeaderViaConfig(t *testing.T) {
	up, seen := captureHeaderStub(t, "X-Grok-Conv-Id")
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "xai", up: up.URL, model: "m1"})
	cfg.Providers[0].SessionHeader = "x-grok-conv-id"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	_, h := newTestServer2(t, cfg)

	for range 2 {
		if w := do(t, h, authClientKey(chatReq(t, "xai/m1"))); w.Code != 200 {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
	}
	if len(*seen) != 2 {
		t.Fatalf("calls=%d", len(*seen))
	}
	if (*seen)[0] == "" || (*seen)[0] != (*seen)[1] {
		t.Fatalf("derived id not stable per key: %v", *seen)
	}
	if !strings.HasPrefix((*seen)[0], "ses_") {
		t.Fatalf("derived id %q must be ses_<hex>", (*seen)[0])
	}

	// The client's own value wins over the derivation.
	r := chatReq(t, "xai/m1")
	r.Header.Set("x-grok-conv-id", "conv-from-client")
	if w := do(t, h, authClientKey(r)); w.Code != 200 {
		t.Fatalf("request failed: %d", w.Code)
	}
	if last := (*seen)[2]; last != "conv-from-client" {
		t.Fatalf("client value lost: got %q", last)
	}
}

// Unknown session_header values must fail config validation, not silently
// drop the ids upstream.
func TestSessionHeaderValidation(t *testing.T) {
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "p1", up: "http://127.0.0.1:1", model: "m1"})
	cfg.Providers[0].SessionHeader = "x grok conv id"
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid header name must fail Validate")
	}
	cfg.Providers[0].SessionHeader = "x-grok-conv-id"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid header name rejected: %v", err)
	}
}
