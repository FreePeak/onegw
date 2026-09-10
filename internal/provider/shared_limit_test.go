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
// Live incident 2026-09-09 10:52: b-ai/glm-5.3-flash surfaced two raw 429s
// ("model Concurrency limit 1200" — an upstream model-wide limit, not a
// per-key one) and a terminal 401 whose body was the upstream's own
// auth/verify service timing out. The ladder must not bench healthy keys
// for shared-limit 429s, and transient verify outages must not surface as
// terminal credential failures.
// ---------------------------------------------------------------------------

const sharedLimitBody = `{"error":{"message":"The request rate exceeds the current model Concurrency limit 1200. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit.","type":"upstream_error","code":1302}}`

const authVerifyBody = `{"error":{"message":"鉴权服务请求失败: Post \"http://ainft-chat-service.apenft-market-production.svc.cluster.local:3210/v1/internal/auth/verify\": read tcp 172.31.74.66:59496->10.100.219.47:3210: read: connection reset by peer","type":"upstream_error","code":401}}`

// mkErrStub answers every request with status/body and counts hits.
func mkErrStub(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newSingleDef builds a one-account pool def pointed at the stub.
func newSingleDef(t *testing.T, srv *httptest.Server, name string) *Def {
	t.Helper()
	p := NewPool()
	def := &Def{Name: name, Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}}}
	p.Set(def)
	return def
}

func TestSharedConcurrency429SkipsLadder(t *testing.T) {
	srv, hits := mkErrStub(t, 429, sharedLimitBody)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]

	_, apiErr := def.Do(context.Background(), a1, "glm-5.3-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 || !apiErr.SharedConcurrency() {
		t.Fatalf("got %+v, want 429 SharedConcurrency", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("shared-limit 429 must not bench the account, cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
	// The pool still hands out the same healthy account for the next
	// request (retry lands on it, unlike a benched key).
	if got, _ := def.NextAccount(""); got == nil || got.Name != "a1" {
		t.Fatalf("pool must keep serving the healthy key, got %+v", got)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}
}

func TestSharedConcurrency429SignatureNarrow(t *testing.T) {
	// An ordinary per-key 429 (no "concurrency limit" phrasing) must keep
	// benching the account on the adaptive ladder — that path is already
	// covered by the ladder tests; here we pin the discriminator.
	srv, _ := mkErrStub(t, 429, `{"error":{"message":"rate limit exceeded, key sk-x","type":"upstream_error"}}`)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]
	if _, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{}`)), false); apiErr == nil || apiErr.SharedConcurrency() {
		t.Fatalf("got %+v, want plain 429 (ladder path)", apiErr)
	}
	slot := findSlot(def.pool, "a1")
	if d := time.Until(slot.cooldown); d <= 0 || d > coolBase {
		t.Fatalf("plain 429 bench=%v, want ~coolBase (ladder base)", d)
	}
}

// Live 2026-09-10 11:10-11:15: the reseller's per-MODEL TPM wall ("The
// request rate exceeds the current model TPM limit 340000000") struck
// kisame, linh.mn and harvey in the SAME second while clone3 served 200s on
// the same model — a shared lane budget, not a per-key limit. Before the
// modelLimitRe classification this body fell into the per-key ladder path:
// the healthy key was benched coolBase..coolCap (shrinking the pool until
// client-visible pool-empty 429s) and the router burned a same-target retry
// into the saturated model. It must skip the ladder exactly like the
// concurrency wall.
const sharedTPMLimitBody = `{"error":{"message":"The request rate exceeds the current model TPM limit 340000000. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit.","type":"upstream_error","code":1302}}`

func TestSharedTPMWall429SkipsLadder(t *testing.T) {
	srv, hits := mkErrStub(t, 429, sharedTPMLimitBody)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]

	_, apiErr := def.Do(context.Background(), a1, "glm-5.3-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 || !apiErr.SharedConcurrency() {
		t.Fatalf("got %+v, want 429 SharedConcurrency (TPM wall)", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("TPM-wall 429 must not bench the account, cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
	if got, _ := def.NextAccount(""); got == nil || got.Name != "a1" {
		t.Fatalf("pool must keep serving the healthy key, got %+v", got)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}
}

func TestAuthVerify401DowngradedToRetryable502(t *testing.T) {
	srv, hits := mkErrStub(t, 401, authVerifyBody)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]

	_, apiErr := def.Do(context.Background(), a1, "glm-5.3-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil {
		t.Fatal("expected error")
	}
	if apiErr.Status != 502 || apiErr.Type != "upstream_auth_verify_failed" {
		t.Fatalf("got %+v, want 502 upstream_auth_verify_failed (transient verify outage)", apiErr)
	}
	if !apiErr.Retryable() {
		t.Fatal("rewritten verify-outage error must be retryable so combos fall through")
	}
	if apiErr.Fallbackable || apiErr.OverQuota() {
		t.Fatalf("verify outage must not mark the account state, got %+v", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() {
		t.Fatalf("healthy key must not be benched for the upstream's verify outage, cooldown=%v", slot.cooldown)
	}
	if !strings.Contains(apiErr.Message, "auth/verify") {
		t.Fatalf("upstream diagnostic must be preserved, got %q", apiErr.Message)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}
}

func TestRealInvalidKey401Untouched(t *testing.T) {
	// A genuine credential refusal must keep its terminal 401 semantics —
	// only the verify-outage transport shape downgrades.
	srv, _ := mkErrStub(t, 401, `{"error":{"message":"Invalid API key","type":"authentication_error","code":401}}`)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]
	_, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 401 {
		t.Fatalf("got %+v, want untouched 401", apiErr)
	}
	if apiErr.Retryable() {
		t.Fatal("real invalid-key 401 must stay terminal")
	}
}

func TestUpstreamRetryAfterRidesErrorObject(t *testing.T) {
	// Upstream-provided Retry-After must reach the error object so
	// Router.Execute never stamps its generic default over it, and the
	// surfaced 429 carries it to the client.
	srv, _ := mkErrStub(t, 429, `{"error":{"message":"rate limit exceeded","type":"upstream_error"}}`)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]
	// mkErrStub cannot set per-test headers; wrap with a header-carrying stub.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"upstream_error"}}`))
	}))
	t.Cleanup(srv2.Close)
	p := NewPool()
	def2 := &Def{Name: "b-ai", Kind: KindOpenAI, BaseURL: srv2.URL,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}}}
	p.Set(def2)

	if _, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{}`)), false); apiErr == nil {
		t.Fatal("expected error from srv")
	}
	_, apiErr := def2.Do(context.Background(), &def2.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.RetryAfter != "7" {
		t.Fatalf("got %+v, want RetryAfter=7 propagated verbatim", apiErr)
	}
}

// Live 2026-09-09 (tokenrouter/z-ai/glm-5.3-free): the engine's
// cold-prefill admission wall ("BackendAdmissionRejected: Engine
// cold-request admission rejected … policies=prefill_pressure …") is a
// shared engine budget, not a per-key limit — the identical request
// succeeded seconds later on the OTHER account. It must skip the
// cooldown ladder exactly like the Tencent concurrency wall: benching
// a healthy credential only shrinks the serving pool.
func TestAdmissionWall429SkipsLadder(t *testing.T) {
	srv, hits := mkErrStub(t, 429, `{"error":{"message":"BackendAdmissionRejected: Engine cold-request admission rejected: dp_rank=0, policies=prefill_pressure, queued_uncached_tokens=0, inflight_uncached_tokens=0, outstanding_uncached_tokens=0, incoming_uncached_tokens=214293, pending_uncached_prefill_tokens=214293","type":"rate_limit_error"}}`)
	def := newSingleDef(t, srv, "tokenrouter")
	a1 := &def.Accounts[0]

	_, apiErr := def.Do(context.Background(), a1, "z-ai/glm-5.3-free", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 || !apiErr.SharedConcurrency() {
		t.Fatalf("got %+v, want 429 SharedConcurrency (admission wall)", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() {
		t.Fatalf("admission wall must not bench the account, cooldown=%v", slot.cooldown)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}
}
