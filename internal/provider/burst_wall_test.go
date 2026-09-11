package provider

// ---------------------------------------------------------------------------
// Burst-wall detection (live 2026-09-11, ring seqs 5960-6000, user-visible
// complaint on b-ai/qwen3.8-flash): the reseller's one-api edge answers
// bursty shared-limit pressure with a raw 429 and an EMPTY body — no
// "concurrency limit"/"TPM limit" wording, no Retry-After, no request-count
// window, so the 103c253 text classifiers see nothing and every such 429
// took the per-key ladder. The ring shows three DIFFERENT accounts 429ing
// within 2s (5977 mnhatlinh, 5978/5979 clone2) while the same accounts
// served 200s seconds later — the cross-account clustering that marks a
// shared lane, not per-key exhaustion. The contract pinned here:
//   - a SECOND distinct account striking the same model inside wallWindow
//     proves the burst: the error becomes shared-wall (Router.Execute
//     falls through to the next combo leg immediately, instead of
//     rotating keys into the same wall), the triggering account is NOT
//     benched (the key is healthy), and the (provider, model) pair parks
//     for wallParkTTL so sibling requests skip re-discovery — model
//     switching right away, per the user's ask;
//   - a single account's 429s stay per-key (ladder bench, account
//     rotation) — the discriminator is cross-account evidence;
//   - the evidence window resets on a proven burst, so a continuing wall
//     must re-prove itself for every park;
//   - ok() keeps a bench stamped during the successful request's flight
//     (the straggler race that un-benched mnhatlinh 4s into its bench and
//     let the same wall re-hit it at 5991) but clears one that predates
//     the request (the #48 deposit-recovery path);
//   - wording-matched model walls (Concurrency/TPM) park the pair too;
//     engine admission walls (request-shaped) never park.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTwoAccountDef(t *testing.T, srv *httptest.Server, name string) *Def {
	t.Helper()
	p := NewPool()
	url := ""
	if srv != nil {
		url = srv.URL
	}
	def := &Def{Name: name, Kind: KindOpenAI, BaseURL: url,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}, {Name: "a2", APIKey: "k2"}}}
	p.Set(def)
	return def
}

func TestBurstWallSecondAccountParksModel(t *testing.T) {
	srv, hits := mkErrStub(t, 429, "")
	def := newTwoAccountDef(t, srv, "b-ai")

	// First 429: per-key hypothesis stands — ladder-bench the account,
	// leave the model alone (a wrong guess costs one benched key, not a
	// parked lane).
	_, err1 := def.Do(context.Background(), &def.Accounts[0], "qwen3.8-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if err1 == nil || err1.SharedConcurrency() {
		t.Fatalf("first empty-body 429 must stay per-key, got %+v", err1)
	}
	if d := time.Until(findSlot(def.pool, "a1").cooldown); d <= 0 || d > coolBase {
		t.Fatalf("first 429 bench=%v, want ~coolBase ladder", d)
	}
	if benched, _ := def.ModelBenched("qwen3.8-flash"); benched {
		t.Fatal("one account's 429 must not park the model")
	}

	// Second DISTINCT account within the window: burst proven.
	_, err2 := def.Do(context.Background(), &def.Accounts[1], "qwen3.8-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if err2 == nil || !err2.SharedConcurrency() {
		t.Fatalf("second distinct account within window must classify shared, got %+v", err2)
	}
	// The trigger's key is healthy: no ladder bench on it.
	if slot := findSlot(def.pool, "a2"); !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("burst-triggering 429 must not bench its account, cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
	// The model parks for one burst window, so Router.Execute skips the
	// combo leg with zero upstream attempts until expiry.
	benched, ready := def.ModelBenched("qwen3.8-flash")
	if !benched {
		t.Fatal("proven burst must park the (provider, model) pair")
	}
	if d := time.Until(ready); d <= 0 || d > wallParkTTL {
		t.Fatalf("park TTL=%v, want within wallParkTTL", d)
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("hits=%d, want 2", atomic.LoadInt32(hits))
	}
}

func TestBurstWallWindowResetsAndExpires(t *testing.T) {
	srv, _ := mkErrStub(t, 429, "")
	def := newTwoAccountDef(t, srv, "b-ai")
	cur := time.Now()
	def.pool.now = func() time.Time { return cur }
	t.Cleanup(func() { def.pool.now = time.Now })

	// Same account twice inside the window: per-key strikes, no burst.
	if _, err := def.Do(context.Background(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false); err == nil || err.SharedConcurrency() {
		t.Fatalf("first strike must stay per-key, got %+v", err)
	}
	cur = cur.Add(time.Second)
	if _, err := def.Do(context.Background(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false); err == nil || err.SharedConcurrency() {
		t.Fatalf("same-account strike must stay per-key, got %+v", err)
	}
	// Past the window: the old sight is stale, so a distinct account
	// records fresh evidence rather than proving a burst.
	cur = cur.Add(wallWindow + time.Second)
	if _, err := def.Do(context.Background(), &def.Accounts[1], "m", nil, bytes.NewReader([]byte(`{}`)), false); err == nil || err.SharedConcurrency() {
		t.Fatalf("stale sight must not prove a burst, got %+v", err)
	}
	if benched, _ := def.ModelBenched("m"); benched {
		t.Fatal("no burst — the model must not be parked")
	}
	// The fresh sight (a2) plus a distinct account inside the new window
	// completes the burst.
	if _, err := def.Do(context.Background(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false); err == nil || !err.SharedConcurrency() {
		t.Fatalf("fresh cross-account pair must prove the burst, got %+v", err)
	}
	// A proven burst RESETS the sight: the immediate next 429 — even from
	// a different account — starts a new evidence cycle instead of
	// latching onto the previous one.
	cur = cur.Add(time.Second)
	if _, err := def.Do(context.Background(), &def.Accounts[1], "m", nil, bytes.NewReader([]byte(`{}`)), false); err == nil || err.SharedConcurrency() {
		t.Fatalf("burst must reset the evidence window, got %+v", err)
	}
}

func TestOKKeepsFreshBenchClearsStaleBench(t *testing.T) {
	def := newTwoAccountDef(t, nil, "b-ai")
	cur := time.Now()
	def.pool.now = func() time.Time { return cur }
	t.Cleanup(func() { def.pool.now = time.Now })

	a1 := &def.Accounts[0]

	// The 5977/5983 race: the request began at t0, a CONCURRENT request
	// 429ed the account and benched it mid-flight, the straggler success
	// completes later — the bench is the fresher verdict and must
	// survive, while the strike count still resets.
	t0 := cur.Add(-3 * time.Second)
	def.pool.rateLimited(a1, 0)
	benchUntil := findSlot(def.pool, "a1").cooldown
	def.pool.ok(a1, t0)
	slot := findSlot(def.pool, "a1")
	if !slot.cooldown.Equal(benchUntil) {
		t.Fatalf("bench stamped during the request's flight must survive its success: got %v, want %v", slot.cooldown, benchUntil)
	}
	if slot.strikes != 0 {
		t.Fatalf("ok() must reset strikes, got %d", slot.strikes)
	}

	// The #48 deposit-recovery shape: the gate benched the account, the
	// deposit landed, and a request that started AFTER the bench
	// succeeds — newer evidence, so the bench clears.
	def.pool.rateLimited(a1, 0)
	def.pool.ok(a1, cur.Add(10*time.Second))
	slot = findSlot(def.pool, "a1")
	if !slot.cooldown.IsZero() || !slot.benchedAt.IsZero() {
		t.Fatalf("bench predating the request must clear on success: cooldown=%v benchedAt=%v", slot.cooldown, slot.benchedAt)
	}

	// Strike reset: two 429s WITHOUT an intervening success double the
	// ladder; with a success between them, the next 429 restarts at
	// coolBase.
	def.pool.rateLimited(a1, 0)
	cur = cur.Add(5 * time.Second)
	def.pool.rateLimited(a1, 0)
	if d := findSlot(def.pool, "a1").cooldown.Sub(cur); d < 20*time.Second-time.Second || d > 20*time.Second {
		t.Fatalf("second consecutive 429 bench=%v, want ~2*coolBase", d)
	}
	def.pool.ok(a1, cur.Add(time.Second))
	def.pool.rateLimited(a1, 0)
	if d := findSlot(def.pool, "a1").cooldown.Sub(cur); d < coolBase-time.Second || d > coolBase+time.Second {
		t.Fatalf("post-success ladder must restart at coolBase, got bench=%v", d)
	}
	// And the account stays skipped while benched.
	if got, _ := def.NextAccount(""); got == nil || got.Name != "a2" {
		t.Fatalf("benched a1 must not be picked, got %+v", got)
	}
}

func TestModelWallMessageParksModel(t *testing.T) {
	srv, hits := mkErrStub(t, 429, sharedTPMLimitBody)
	def := newSingleDef(t, srv, "b-ai")

	_, err := def.Do(context.Background(), &def.Accounts[0], "glm-5.3-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if err == nil || !err.SharedConcurrency() || !err.ModelWall() {
		t.Fatalf("TPM wall must classify shared + model-wall, got %+v", err)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("wording-matched wall must not bench the account, cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
	benched, ready := def.ModelBenched("glm-5.3-flash")
	if !benched {
		t.Fatal("wording-matched model wall must park the pair")
	}
	if d := time.Until(ready); d <= 0 || d > wallParkTTL {
		t.Fatalf("park TTL=%v, want within wallParkTTL", d)
	}
	if got, _ := def.NextAccount(""); got == nil || got.Name != "a1" {
		t.Fatalf("healthy key must stay in the pool, got %+v", got)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}
}

func TestAdmissionWallDoesNotParkModel(t *testing.T) {
	// Engine admission rejections are per-request prefill-shape verdicts:
	// other requests (warm cache) keep serving the same model, so the
	// pair must NOT park — only the per-request fall-through applies.
	srv, hits := mkErrStub(t, 429, `{"error":{"message":"BackendAdmissionRejected: Engine cold-request admission rejected: dp_rank=0, policies=prefill_pressure, incoming_uncached_tokens=214293","type":"rate_limit_error"}}`)
	def := newSingleDef(t, srv, "tokenrouter")

	_, err := def.Do(context.Background(), &def.Accounts[0], "z-ai/glm-5.3-free", nil, bytes.NewReader([]byte(`{}`)), false)
	if err == nil || !err.SharedConcurrency() {
		t.Fatalf("admission wall must classify shared, got %+v", err)
	}
	if err.ModelWall() {
		t.Fatal("admission wall is request-shaped, not a model wall")
	}
	if benched, _ := def.ModelBenched("z-ai/glm-5.3-free"); benched {
		t.Fatal("admission wall must not park the model")
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}
}
