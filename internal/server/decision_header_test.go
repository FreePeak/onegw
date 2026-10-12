package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"onegw/internal/config"
)

// okUpstream answers a plain OpenAI chat completion, recording whether it
// ever saw the X-OneGW-Decision header on its request.
func okUpstream(t *testing.T, leaked *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if strings.EqualFold(k, DecisionHeader) {
				atomic.AddInt32(leaked, 1)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-decision", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rateLimitedUpstream answers every request with an upstream 429 (adaptive
// ladder benches the account, so the combo falls through immediately).
func rateLimitedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A direct route must stamp X-OneGW-Decision with the serving provider,
// account, and upstream model before the body write, and must NOT claim a
// fallback: request= is the client's own string and the serving target IS
// that route's first target.
func TestDecisionHeaderDirectRoute(t *testing.T) {
	var leaked int32
	up := okUpstream(t, &leaked)
	_, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "acct1", APIKey: "key-ok"}},
	}))

	w := do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("got %d %s, want completion served", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get(DecisionHeader),
		"provider=p1; account=acct1; model=m1; attempts=1; request=p1/m1; fallback=false"; got != want {
		t.Fatalf("X-OneGW-Decision = %q, want %q", got, want)
	}
}
// On a combo whose first target 429s upstream, the header must name the
// SECOND (serving) provider — failed attempts never wrote a body, so the
// winner's pre-body stamp is what the client sees — and must say so:
// fallback=true is the half a client needs to tell the user "answered by
// something other than what you asked for". The client's own string stays in
// request=, so a combo NAME is never mistaken for the model that served.
func TestDecisionHeaderComboNamesWinner(t *testing.T) {
	limited := rateLimitedUpstream(t)
	var leaked int32
	ok := okUpstream(t, &leaked)
	_, h := newTestServer2(t, gatedCfg(t,
		config.ProviderCfg{
			Name: "p1", Kind: "openai", BaseURL: limited.URL, Models: []string{"m1"},
			Accounts: []config.Acct{{Name: "rl", APIKey: "key-rl"}},
		},
		config.ProviderCfg{
			Name: "p2", Kind: "openai", BaseURL: ok.URL, Models: []string{"m2"},
			Accounts: []config.Acct{{Name: "ok", APIKey: "key-ok"}},
		},
	))

	w := do(t, h, authSk(chatReq(t, "pair")))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("got %d %s, want fall-through served by p2", w.Code, w.Body.String())
	}
	got := w.Header().Get(DecisionHeader)
	if !strings.HasPrefix(got, "provider=p2; account=ok; model=m2; attempts=2; request=pair; fallback=true") {
		t.Fatalf("X-OneGW-Decision = %q, want the serving provider p2's trace with request=pair fallback=true", got)
	}
}

// A retry onto the SAME target is not a fallback: attempts counts it, but
// the model the user asked for is the model that answered. Pinned here
// because attempts>1 is the tempting (and wrong) client-side test.
func TestDecisionHeaderSameTargetRetryIsNotFallback(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"flaky","type":"api_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	_, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "acct1", APIKey: "key-ok"}},
	}))

	w := do(t, h, authSk(chatReq(t, "p1/m1")))
	got := w.Header().Get(DecisionHeader)
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Skipf("upstream answered %d call(s); the retry ladder did not re-attempt (nothing to pin)", n)
	}
	if !strings.Contains(got, "; fallback=false") {
		t.Fatalf("X-OneGW-Decision = %q, want fallback=false for a same-target retry", got)
	}
}

// The decision header is a response header only: the upstream request must
// never carry it.
func TestDecisionHeaderNeverSentUpstream(t *testing.T) {
	var leaked int32
	up := okUpstream(t, &leaked)
	_, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "acct1", APIKey: "key-ok"}},
	}))

	w := do(t, h, authSk(chatReq(t, "p1/m1")))
	if w.Code != 200 {
		t.Fatalf("got %d %s, want completion", w.Code, w.Body.String())
	}
	if n := atomic.LoadInt32(&leaked); n != 0 {
		t.Fatalf("X-OneGW-Decision leaked upstream on %d request(s)", n)
	}
}
