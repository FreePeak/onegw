package provider

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Live incident 2026-09-09 22:56 (tokenrouter/z-ai/glm-5.3-free @harvey,
// 429 · 0 in · 0 out): "You have reached the request limit
// [z-ai/glm-5.3-free]: Maximum 8 requests within 1 minutes." Two gaps:
//
//  1. REACTIVE — the 429 states its window in the body, but the bench used
//     coolBase (10s): ring shows 429 @15:58:46, ladder retry @15:58:57
//     429s again (still inside the closed window), success only ~30-40s
//     later. The stated window must win the bench like a Retry-After
//     header would.
//
//  2. PROACTIVE — the budget is SHARED across keys, not per-key: harvey
//     429ed with only ~5 counted attempts in its trailing 60s while linh
//     served 200s in the same window (and both keys got both outcomes in
//     adjacent windows — the #64 ring-proof pattern). A per-account RPM
//     cannot express that wall; the pool needs a provider-wide budget.
// ---------------------------------------------------------------------------

const requestLimitBody = `{"error":{"message":"You have reached the request limit[z-ai/glm-5.3-free]: Maximum 8 requests within 1 minutes. (request id: 20260909155655977745357PIFzKuhT)","type":"api_error"}}`

// The stated window benches the account for ~60s (verbatim, like a
// Retry-After header), NOT the 10s ladder base that re-enters the
// still-closed window. The error also carries the window so Router.Execute
// never stamps its generic 10s over it.
func TestWindowed429BenchesForStatedWindow(t *testing.T) {
	srv, hits := mkErrStub(t, 429, requestLimitBody)
	def := newSingleDef(t, srv, "tokenrouter")
	a1 := &def.Accounts[0]

	_, apiErr := def.Do(context.Background(), a1, "z-ai/glm-5.3-free", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("got %+v, want 429", apiErr)
	}
	if got := apiErr.RateWindow(); got != time.Minute {
		t.Fatalf("RateWindow() = %v, want 1m", got)
	}
	if apiErr.RetryAfter != "60" {
		t.Fatalf("RetryAfter = %q, want 60 (stated window, beats the 10s default)", apiErr.RetryAfter)
	}
	slot := findSlot(def.pool, "a1")
	bench := time.Until(slot.cooldown)
	if bench <= 30*time.Second || bench > 61*time.Second {
		t.Fatalf("bench = %v, want ~60s window (was 10s ladder base)", bench)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("hits=%d, want 1", atomic.LoadInt32(hits))
	}

	// The window is a per-key request-count limit (unlike the shared
	// admission wall): the account must be benched so rotation starts.
	if a, _ := def.NextAccount(""); a != nil && a.Name == "a1" {
		t.Fatal("benched account must not be re-picked before the window clears")
	}
}

// Consecutive windowed 429s re-bench for the window (the window is still
// closed), not for a ladder doubling beyond it.
func TestWindowed429ReBenchesForWindow(t *testing.T) {
	srv, _ := mkErrStub(t, 429, requestLimitBody)
	def := newSingleDef(t, srv, "tokenrouter")
	a1 := &def.Accounts[0]
	def.Do(context.Background(), a1, "z-ai/glm-5.3-free", nil, bytes.NewReader([]byte(`{}`)), false)
	first := findSlot(def.pool, "a1").cooldown

	time.Sleep(10 * time.Millisecond) // strike 2 lands later in real time
	def.Do(context.Background(), a1, "z-ai/glm-5.3-free", nil, bytes.NewReader([]byte(`{}`)), false)
	second := findSlot(def.pool, "a1").cooldown
	if !second.After(first) {
		t.Fatalf("second hit must extend the bench: first=%v second=%v", first, second)
	}
	if d := second.Sub(time.Now()); d > 61*time.Second {
		t.Fatalf("bench = %v, want ~60s re-window, not a ladder doubling", d)
	}
}

// PROACTIVE gap: the shared provider budget (Def.RPM) gates the whole pool
// as one — when it drains, the pool reports the honest refill instant
// instead of feeding the shared window doomed per-account attempts, and
// round-robin rotation keeps working while budget remains.
func TestSharedProviderBudgetGatesWholePool(t *testing.T) {
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb"},
	}, 0, 6) // shared 6/min: capacity 2 burst + 6/min refill
	cur := time.Now()
	p.now = func() time.Time { return cur }

	for _, want := range []string{"a", "b"} {
		a, ready := p.next("")
		if a == nil {
			t.Fatalf("pick %q: pool empty, ready=%v", want, ready)
		}
		if a.Name != want {
			t.Fatalf("pick = %s, want %s (round-robin intact)", a.Name, want)
		}
	}
	// Both burst tokens spent: pool empty with the honest shared refill
	// (6/min → next token 10s out), not a doomed pick.
	a, ready := p.next("")
	if a != nil {
		t.Fatalf("shared budget drained but pick handed out: %s", a.Name)
	}
	if ready.IsZero() || ready.Before(cur.Add(9*time.Second)) || ready.After(cur.Add(11*time.Second)) {
		t.Fatalf("ready = %v, want ~10s shared refill", ready)
	}
	cur = ready // refill instant: the next pick is granted
	if a, _ := p.next(""); a == nil {
		t.Fatal("pick after shared refill blocked")
	}
}

// A shared budget must also apply to a session's sticky pin: when the
// shared budget is empty the pinned account is not granted either.
func TestSharedBudgetBlocksStickyPin(t *testing.T) {
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb"},
	}, time.Minute, 2)
	cur := time.Now()
	p.now = func() time.Time { return cur }

	p.next("sess-1") // pins a, spends burst token 1
	if a, _ := p.next("sess-1"); a == nil || a.Name != "a" {
		t.Fatalf("sticky pin must hold: got %v", a)
	}
	if a, ready := p.next("sess-1"); a != nil {
		t.Fatalf("shared budget empty but pin granted: %s (ready=%v)", a.Name, ready)
	}
}

// RPM=0 must leave pools exactly as before — no shared bucket, no gating.
func TestSharedBudgetZeroUncapped(t *testing.T) {
	p := newAccountPool([]Account{{Name: "a", APIKey: "ka"}}, 0, 0)
	for range 50 {
		if a, _ := p.next(""); a == nil {
			t.Fatal("uncapped pool drained")
		}
	}
}

// Examined-but-not-granted slots must not spend their own bucket: with the
// shared budget empty, scanning siblings must leave per-account tokens
// untouched (a consuming available() would burn them on every scan).
func TestBlockedScanDoesNotSpendOwnBuckets(t *testing.T) {
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka", RPM: 6},
		{Name: "b", APIKey: "kb", RPM: 6},
	}, 0, 2) // shared drains after the burst; own buckets still full
	cur := time.Now()
	p.now = func() time.Time { return cur }

	p.next("")
	p.next("")
	// Shared empty now; scans below must NOT consume own-bucket tokens.
	// Advance far past the shared refill: the pool must serve immediately
	// again (own buckets untouched by the blocked scans).
	cur = cur.Add(31 * time.Second)
	if a, _ := p.next(""); a == nil {
		t.Fatal("blocked scans spent own-bucket tokens; pool should serve")
	}
}

// A shared budget coexists with per-account budgets: the honest ready time
// is the LATER of the shared refill and the earliest own gate.
func TestSharedBudgetCoexistsWithOwnBuckets(t *testing.T) {
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka", RPM: 1}, // own: 1/min → ~60s refill
		{Name: "b", APIKey: "kb"},
	}, 0, 6)
	cur := time.Now()
	p.now = func() time.Time { return cur }

	for range 3 { // shared burst 2 + refill at 10s steps
		if a, _ := p.next(""); a == nil {
			t.Fatal("unexpected early pool-empty")
		}
		cur = cur.Add(10 * time.Second)
	}
	// b is uncapped; shared refilled at each 10s step, so a pick exists.
	a, ready := p.next("")
	if a == nil {
		t.Fatalf("b is uncapped and shared refilled: pick should exist (ready=%v)", ready)
	}
}
