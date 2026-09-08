package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Issue #36: client session-affinity header forwarding + derived per-key id.
// ---------------------------------------------------------------------------

// mkAffinityStub answers 200 and records the named upstream header per call.
func mkAffinityStub(t *testing.T, header string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(header))
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestDoForwardsClientSessionHeadersVerbatim(t *testing.T) {
	srv, seen := mkAffinityStub(t, "X-Grok-Conv-Id")
	def := &Def{Name: "xai", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "k1", APIKey: "k1"}}}

	// The client's own casing must match case-insensitively: Do builds a
	// fresh request, so the value can only ride the forwarded header.
	hdr := http.Header{}
	hdr.Set("x-grok-conv-id", "conv-abc")
	if _, apiErr := def.Do(t.Context(), &def.Accounts[0], "m", hdr, bytes.NewReader([]byte(`{}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if len(*seen) != 1 || (*seen)[0] != "conv-abc" {
		t.Fatalf("upstream saw %v, want [conv-abc] forwarded verbatim", *seen)
	}
}

func TestDoForwardsEveryAllowListedHeader(t *testing.T) {
	srv, seen := mkAffinityStub(t, "X-Session-Id")
	def := &Def{Name: "p", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "k1", APIKey: "k1"}}}
	hdr := http.Header{}
	hdr.Set("x-session-id", "sid-1")
	hdr.Set("session_id", "sess-raw")
	hdr.Set("x-unrelated", "nope")
	if _, apiErr := def.Do(t.Context(), &def.Accounts[0], "m", hdr, bytes.NewReader([]byte(`{}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if len(*seen) != 1 || (*seen)[0] != "sid-1" {
		t.Fatalf("x-session-id = %v, want [sid-1]", *seen)
	}
	// session_id (raw snake_case name) rides too; assert via a second stub
	// pass on the generic recorder.
	srv2, seen2 := mkAffinityStub(t, "Session_Id")
	def2 := &Def{Name: "p", Kind: KindOpenAI, BaseURL: srv2.URL,
		Accounts: []Account{{Name: "k1", APIKey: "k1"}}}
	hdr2 := http.Header{}
	hdr2.Set("session_id", "sess-raw")
	if _, apiErr := def2.Do(t.Context(), &def2.Accounts[0], "m", hdr2, bytes.NewReader([]byte(`{}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if len(*seen2) != 1 || (*seen2)[0] != "sess-raw" {
		t.Fatalf("session_id = %v, want [sess-raw]", *seen2)
	}
}

func TestDoDerivedSessionHeaderOnlyWhenGated(t *testing.T) {
	// Ungated provider: a missing client value must NOT invent a header.
	srv, seen := mkAffinityStub(t, "X-Grok-Conv-Id")
	def := &Def{Name: "plain", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "k1", APIKey: "k1"}}}
	if _, apiErr := def.Do(t.Context(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if len(*seen) != 1 || (*seen)[0] != "" {
		t.Fatalf("ungated provider invented header: %v", *seen)
	}

	// Gated provider (SessionHeader set): the derived id is stable per key
	// and differs across keys — the cache-affinity trade.
	defSess := &Def{Name: "xai", Kind: KindOpenAI, BaseURL: srv.URL, SessionHeader: "x-grok-conv-id",
		Accounts: []Account{{Name: "k1", APIKey: "key-a"}, {Name: "k2", APIKey: "key-b"}}}
	for _, key := range []string{"key-a", "key-b", "key-a"} {
		acct := defSess.Accounts[0]
		if key == "key-b" {
			acct = defSess.Accounts[1]
		}
		if _, apiErr := defSess.Do(t.Context(), &acct, "m", nil, bytes.NewReader([]byte(`{}`)), false); apiErr != nil {
			t.Fatalf("Do(%s): %+v", key, apiErr)
		}
	}
	got := *seen
	if got[1] == "" || got[2] == "" || got[3] == "" {
		t.Fatalf("gated provider did not derive ids: %v", got)
	}
	if !strings.HasPrefix(got[1], "ses_") || len(got[1]) != len("ses_")+32 {
		t.Fatalf("derived id %q must be ses_<32 hex>", got[1])
	}
	if got[1] != got[3] {
		t.Fatalf("same key derived %q then %q, want stable", got[1], got[3])
	}
	if got[1] == got[2] {
		t.Fatal("different keys derived the same id")
	}

	// A client-sent value always beats the derivation.
	hdr := http.Header{}
	hdr.Set("X-Grok-Conv-Id", "conv-from-client")
	if _, apiErr := defSess.Do(t.Context(), &defSess.Accounts[0], "m", hdr, bytes.NewReader([]byte(`{}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if last := (*seen)[len(*seen)-1]; last != "conv-from-client" {
		t.Fatalf("client value lost: got %q", last)
	}
}

// ---------------------------------------------------------------------------
// Issue #48: gated 403 benches the account and falls back; other 403s don't.
// ---------------------------------------------------------------------------

// mkGatedStub answers the given status/body for the key named by the
// Authorization header while *gated is true (so a deposit can be simulated
// by clearing the flag), 200 otherwise.
func mkGatedStub(t *testing.T, failKey string, status int, body string) (*httptest.Server, *int32, *bool) {
	t.Helper()
	var hits int32
	gated := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if gated && r.Header.Get("Authorization") == "Bearer "+failKey {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &gated
}

const gatedBody = `{"error":{"message":"Access restricted. Deposit required to unlock premium models.","type":"access_denied","code":"access_denied"}}`

func TestGated403BenchesAccountAndFallsBack(t *testing.T) {
	srv, hits, gated := mkGatedStub(t, "k1", 403, gatedBody)
	p := NewPool()
	def := &Def{Name: "b-ai", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "gated", APIKey: "k1"}, {Name: "healthy", APIKey: "k2"}}}
	p.Set(def)

	// The gated key's call fails with a Fallbackable 403 — and the
	// account is benched on the ladder.
	res, apiErr := def.Do(context.Background(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	if apiErr == nil || apiErr.Status != 403 || !apiErr.Fallbackable {
		t.Fatalf("got %+v, want 403 Fallbackable", apiErr)
	}
	if res != nil {
		t.Fatal("error must not carry a result")
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("upstream hits=%d, want 1", atomic.LoadInt32(hits))
	}
	slot := findSlot(def.pool, "gated")
	if d := time.Until(slot.cooldown); d <= 0 || d > coolBase {
		t.Fatalf("gated bench=%v, want ~coolBase (ladder base)", d)
	}
	// The pool now hands out the healthy account.
	if got, _ := def.NextAccount(""); got.Name != "healthy" {
		t.Fatalf("pool served %s, want the healthy account", got.Name)
	}
	// A deposit clears the gate: the next success resets the bench and
	// the ladder (pool.ok), exactly like a recovered 429 key.
	*gated = false
	if _, apiErr := def.Do(context.Background(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false); apiErr != nil {
		t.Fatalf("post-recovery Do: %+v", apiErr)
	}
	slot = findSlot(def.pool, "gated")
	if !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("success must reset the bench, got cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
}

func TestNonGated403FailsFast(t *testing.T) {
	// Invalid-key-style 403: no signature match, no bench, no Fallbackable.
	srv, _, _ := mkGatedStub(t, "k1", 403, `{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`)
	p := NewPool()
	def := &Def{Name: "p", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}}}
	p.Set(def)
	if _, apiErr := def.Do(context.Background(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false); apiErr == nil || apiErr.Status != 403 || apiErr.Fallbackable {
		t.Fatalf("got %+v, want plain 403 (fail fast)", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() {
		t.Fatalf("plain 403 must not bench the account, cooldown=%v", slot.cooldown)
	}
}

func TestGated403LadderEscalates(t *testing.T) {
	// Gating is account state: a second gated hit on the same account must
	// extend the window (adaptive ladder), like consecutive 429s.
	srv, _, _ := mkGatedStub(t, "k1", 403, gatedBody)
	def := &Def{Name: "b-ai", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "gated", APIKey: "k1"}}}
	p := NewPool()
	p.Set(def)
	a1 := &def.Accounts[0]
	for i := range 2 {
		if _, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false); apiErr == nil || !apiErr.Fallbackable {
			t.Fatalf("hit %d: got %+v, want Fallbackable 403", i, apiErr)
		}
	}
	slot := findSlot(def.pool, "gated")
	if d := time.Until(slot.cooldown); d <= coolBase || d > 2*coolBase {
		t.Fatalf("second gated hit bench=%v, want ~2x coolBase (escalated)", d)
	}
}

func TestGated403SignatureNarrow(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{403, `{"error":{"type":"access_denied","message":"Deposit required to unlock premium models."}}`, true},
		{403, `{"error":{"type":"forbidden","message":"Access restricted. Deposit required to unlock premium models."}}`, true}, // body-only match
		{403, `{"error":{"type":"permission_error","message":"not allowed"}}`, false},
		{403, `{"error":{"type":"invalid_request_error","message":"Invalid API key"}}`, false},
		{401, `{"error":{"type":"access_denied","message":"bad key"}}`, false},
		{429, `{"error":{"type":"rate_limit","message":"slow down"}}`, false},
	}
	for i, tc := range cases {
		if got := gated403(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("case %d: gated403(%d, %q) = %v, want %v", i, tc.status, tc.body, got, tc.want)
		}
	}
}
