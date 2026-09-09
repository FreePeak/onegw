package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onegw/internal/config"
)

// captureBodyStub answers a completion and records each raw upstream body.
func captureBodyStub(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// wireCacheProfileCfg builds a config whose single provider carries the
// given cache_profile, mirroring what the TOML loader produces.
func wireCacheProfileCfg(t *testing.T, up, profile string) *config.Config {
	t.Helper()
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "p1", up: up, model: "m1"})
	cfg.Providers[0].CacheProfile = profile
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

// The attempt choke point anchors cache markers LAST (issue #34): a
// sticky-key profiled provider's buffered request must arrive upstream
// with prompt_cache_key = the client session identity, and a request
// without one must not invent a key.
func TestWiredStickyKeyAnchorsBufferedBody(t *testing.T) {
	up, seen := captureBodyStub(t)
	_, h := newTestServer2(t, wireCacheProfileCfg(t, up.URL, "sticky-key"))

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer sk-client")
	r.Header.Set("X-Opencode-Session", "conv-abc")
	if w := do(t, h, r); w.Code != 200 {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte((*seen)[0]), &got); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if got["prompt_cache_key"] != "s:conv-abc" {
		t.Fatalf("prompt_cache_key = %v, want s:conv-abc", got["prompt_cache_key"])
	}

	// An authenticated request without a session header still has the
	// key-label identity, so injection continues with that key.
	if w := do(t, h, authClientKey(chatReq(t, "p1/m1"))); w.Code != 200 {
		t.Fatalf("second request failed: %d", w.Code)
	}
	if len(*seen) != 2 {
		t.Fatalf("upstream saw %d bodies", len(*seen))
	}
	if !strings.Contains((*seen)[1], `"prompt_cache_key":"k:`) {
		t.Fatalf("key-label identity not used as cache key: %s", (*seen)[1])
	}
}

// A provider with profile none (the default) must reach upstream
// byte-identical — no cache bytes are ever invented for GLM/DeepSeek/b-ai
// style upstreams, wired or not.
func TestWiredNoneProfileLeavesBodyUntouched(t *testing.T) {
	up, seen := captureBodyStub(t)
	cfg := makeCfg(t, "sk-client", "", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	_, h := newTestServer2(t, cfg)

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer sk-client")
	r.Header.Set("X-Opencode-Session", "conv-abc")
	if w := do(t, h, r); w.Code != 200 {
		t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains((*seen)[0], "prompt_cache_key") {
		t.Fatalf("none profile invented cache bytes: %s", (*seen)[0])
	}
}

// The stream fast path bypasses prepareUpstreamBody, so a cache-profiled
// provider must be herded into the buffered pipeline where anchoring
// runs (issue #34): the stub's non-streaming reply exercises the
// fallback, and the body still must be anchored.
func TestWiredProfiledProviderStreamsThroughBufferedPipeline(t *testing.T) {
	up, seen := captureBodyStub(t)
	_, h := newStreamServer(t, streamCfg(t, false, func(c *config.Config) {
		c.Providers[0].CacheProfile = "sticky-key"
	}, providerSpec{name: "p1", up: up.URL, model: "m1"}))

	body := `{"model":"p1/m1","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	r.Header.Set("X-Opencode-Session", "conv-stream")
	w := do(t, h, r)
	if w.Code != 200 {
		t.Fatalf("stream request failed: %d %s", w.Code, w.Body.String())
	}
	// Buffered fallback served it: one JSON completion body captured.
	if len(*seen) != 1 {
		t.Fatalf("expected one buffered upstream body, saw %d", len(*seen))
	}
	if !strings.Contains((*seen)[0], `"prompt_cache_key":"s:conv-stream"`) {
		t.Fatalf("buffered fallback did not anchor: %s", (*seen)[0])
	}
}
