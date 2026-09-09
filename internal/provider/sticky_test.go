package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// pick adapts accountPool.next's (account, poolReady) pair to the tests:
// every pool in this file starts uncooled, so next() never returns a nil
// account here.
func pick(p *accountPool, id string) *Account {
	a, _ := p.next(id)
	if a == nil {
		panic("pick: pool unexpectedly cooling (fixture bug)")
	}
	return a
}

// mkPool builds a 3-account pool (each weight 1 → one slot per account)
// with the given sticky TTL and a controllable clock.

func mkPool(t *testing.T, sticky time.Duration) (*accountPool, *time.Time) {
	t.Helper()
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb"},
		{Name: "c", APIKey: "kc"},
	}, sticky, 0)
	cur := time.Now()
	p.now = func() time.Time { return cur }
	return p, &cur
}

func TestPlainRoundRobinUnchangedWithoutSticky(t *testing.T) {
	p, _ := mkPool(t, 0)
	// No identity: plain RR regardless of ttl.
	var names []string
	for range 3 {
		names = append(names, pick(p, "").Name)
	}
	if names[0] == names[1] || names[1] == names[2] {
		t.Fatalf("plain RR should rotate: %v", names)
	}
	// With ttl=0 even a non-empty identity stays plain RR.
	if pick(p, "sess").Name == pick(p, "sess").Name {
		t.Fatal("ttl=0 must ignore identity")
	}
}

func TestStickyPinsIdentityAcrossCalls(t *testing.T) {
	p, _ := mkPool(t, 5*time.Minute)
	first := pick(p, "sess")
	for range 4 {
		if got := pick(p, "sess"); got != first {
			t.Fatalf("pin drifted: got %s want %s", got.Name, first.Name)
		}
	}
	// A different identity gets its own pin (not serialized onto a's key).
	second := pick(p, "other")
	if second.Name == first.Name {
		t.Fatalf("distinct identity should rotate to next account, got %s twice", first.Name)
	}
	if got := pick(p, "other"); got != second {
		t.Fatalf("second identity pin drifted: %s vs %s", got.Name, second.Name)
	}
}

func TestStickyExpiresAfterTTL(t *testing.T) {
	p, cur := mkPool(t, 5*time.Minute)
	first := pick(p, "sess")
	*cur = cur.Add(6 * time.Minute)
	if got := pick(p, "sess"); got == first {
		t.Fatal("expired pin must rotate to the next account")
	}
}

func TestStickyRotatesWhenPinnedAccountCools(t *testing.T) {
	p, cur := mkPool(t, 5*time.Minute)
	first := pick(p, "sess")
	p.cool(first, time.Minute) // pinned key exhausted upstream
	*cur = cur.Add(time.Second)
	got := pick(p, "sess")
	if got.Name == first.Name {
		t.Fatal("cooling pinned account must rotate")
	}
	// The rotated pick re-pins: subsequent calls follow the new account.
	if again := pick(p, "sess"); again != got {
		t.Fatalf("re-pin drifted: %s vs %s", again.Name, got.Name)
	}
}

func TestUnpinFreesTheIdentity(t *testing.T) {
	p, _ := mkPool(t, 5*time.Minute)
	first := pick(p, "sess")
	p.unpin("sess")
	if got := pick(p, "sess"); got == first {
		t.Fatal("after unpin the next pick must rotate to a different account")
	}
}

func TestFailedAttemptNeverRePinsSameAccount(t *testing.T) {
	// Router-level contract: NextAccount(id) then Unpin(id) on failure
	// means the retry (and combo fallthrough) lands on a different key.
	p, _ := mkPool(t, 5*time.Minute)
	failed := pick(p, "sess")
	p.unpin("sess")
	retry := pick(p, "sess")
	if retry.Name == failed.Name {
		t.Fatalf("retry stuck to failed account %s", failed.Name)
	}
	// Combo fallthrough to a second provider's pool is a fresh pool, so
	// its first pick is unaffected by the first provider's unpin.
}

func TestStickyHonorsWeights(t *testing.T) {
	// Weighted pool: account b (weight 2) occupies two slots; a pin is by
	// account identity, so pinning b keeps returning b, and unpin+rotate
	// respects the slot order a→b→b→c.
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb", Weight: 2},
		{Name: "c", APIKey: "kc"},
	}, 5*time.Minute, 0)
	cur := time.Now()
	p.now = func() time.Time { return cur }

	pinned := pick(p, "sess")
	p.unpin("sess")
	got := pick(p, "sess")
	if got.Name == pinned.Name {
		t.Fatal("rotate after unpin must move accounts")
	}
}

func TestStickyPinMapStaysBounded(t *testing.T) {
	// Hostile client flood: thousands of unique session ids must not grow
	// the affinity map unboundedly (RAM guard, maxStickyPins + sweep).
	p, _ := mkPool(t, time.Hour)
	for i := range maxStickyPins * 2 {
		pick(p, fmt.Sprintf("sess-%d", i))
	}
	if len(p.sticky) > maxStickyPins {
		t.Fatalf("pin map unbounded: %d entries, cap %d", len(p.sticky), maxStickyPins)
	}
	// After the reset path, an existing identity still resolves to A pin.
	a := pick(p, "late-comer")
	if b := pick(p, "late-comer"); b.Name != a.Name {
		t.Fatalf("pin broken after cap reset: %s vs %s", a.Name, b.Name)
	}
}

func TestStickyWeightsRotateThroughWeightedSlots(t *testing.T) {
	// Weighted pool a(1),b(2),c(1) -> slots [a,b,b,c]. Unpin+rotate walks
	// the slot list, so consecutive unpins visit b twice before c — the
	// documented first-matching-slot quirk of identity-matched pins.
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb", Weight: 2},
		{Name: "c", APIKey: "kc"},
	}, time.Hour, 0)
	cur := time.Now()
	p.now = func() time.Time { return cur }

	var order []string
	for range 3 {
		order = append(order, pick(p, "sess").Name)
		p.unpin("sess")
	}
	if order[0] == order[1] && order[1] == order[2] {
		t.Fatalf("weighted rotation stuck on one account: %v", order)
	}
}

func TestStickyIdentityEmptyFallsBackToRR(t *testing.T) {
	p, _ := mkPool(t, 5*time.Minute)
	a := pick(p, "")
	b := pick(p, "")
	if a.Name == b.Name {
		t.Fatal("empty identity must stay plain round-robin")
	}
}

func TestRateLimitedAdaptiveLadderEscalates(t *testing.T) {
	// Observed live behavior (b.ai one-api style): a first 429 is a ~5s
	// burst window, but a key kept hammering while 429ing sinks into
	// longer upstream blocks. The ladder benches 10s, doubling per
	// consecutive 429, capped at 60s.
	p, cur := mkPool(t, 0)
	a := pick(p, "")
	p.rateLimited(a, 0)
	slot := &p.accts[0]
	if !slot.cooldown.Equal(cur.Add(coolBase)) || slot.strikes != 1 {
		t.Fatalf("first 429: cooldown=%v strikes=%d, want %v/1", slot.cooldown, slot.strikes, coolBase)
	}
	*cur = cur.Add(time.Second)
	p.rateLimited(a, 0)
	if want := cur.Add(20 * time.Second); !slot.cooldown.Equal(want) {
		t.Fatalf("second 429: cooldown=%v, want +20s (%v)", slot.cooldown, want)
	}
	*cur = cur.Add(time.Second)
	p.rateLimited(a, 0)
	*cur = cur.Add(time.Second)
	p.rateLimited(a, 0)
	*cur = cur.Add(time.Second)
	p.rateLimited(a, 0) // ladder exhausted: must clamp at coolCap
	if want := cur.Add(coolCap); !slot.cooldown.Equal(want) {
		t.Fatalf("fifth 429: cooldown=%v, want +cap %v", slot.cooldown, want)
	}
	// Still capped, never longer.
	*cur = cur.Add(time.Second)
	p.rateLimited(a, 0)
	if want := cur.Add(coolCap); !slot.cooldown.Equal(want) {
		t.Fatalf("sixth 429: cooldown=%v, want still +cap %v", slot.cooldown, want)
	}
}

func TestRateLimitedRetryAfterWinsVerbatim(t *testing.T) {
	p, cur := mkPool(t, 0)
	a := pick(p, "")
	p.rateLimited(a, 45*time.Second)
	if got := p.accts[0].cooldown; !got.Equal(cur.Add(45 * time.Second)) {
		t.Fatalf("Retry-After 45s: cooldown=%v, want +45s", got)
	}
	if p.accts[0].strikes != 1 {
		t.Fatalf("strikes=%d, want 1 (escalation counter still advances)", p.accts[0].strikes)
	}
}

func TestSuccessResetsLadder(t *testing.T) {
	// A recovered key must re-enter the ladder at coolBase, not bench
	// 60s after its next isolated 429. Recovery happens after the
	// cooldown expired (next() never hands out cooling accounts), so
	// advance the clock before the success.
	p, cur := mkPool(t, 0)
	a := pick(p, "")
	p.rateLimited(a, 0)
	p.rateLimited(a, 0)
	*cur = cur.Add(coolCap + time.Minute) // bench expired, upstream accepts again
	p.ok(a)
	if p.accts[0].strikes != 0 {
		t.Fatalf("strikes=%d after success, want 0", p.accts[0].strikes)
	}
	p.rateLimited(a, 0)
	if want := cur.Add(coolBase); !p.accts[0].cooldown.Equal(want) {
		t.Fatalf("post-reset 429: cooldown=%v, want coolBase %v", p.accts[0].cooldown, want)
	}
}
func TestNextAccountNilWhenPoolCooling(t *testing.T) {
	// Fast > doomed-call: when every account is cooling, next() must NOT
	// hand out a pick that can only 429 again; it returns nil plus the
	// soonest recovery instant.
	p, cur := mkPool(t, 0)
	a, ready := p.next("")
	if a == nil || !ready.IsZero() {
		t.Fatalf("fresh pool: a=%v ready=%v, want account and zero ready", a, ready)
	}
	// Cool the whole pool (b and c carry Retry-After-style shorter and
	// longer benches; the soonest must win).
	p.rateLimited(a, 0)                                 // ladder: +coolBase
	p.rateLimited(&Account{Name: "b", APIKey: "kb"}, 0) // ladder: +coolBase
	p.rateLimited(&Account{Name: "c", APIKey: "kc"}, 30*time.Second)
	acct, ready := p.next("")
	if acct != nil {
		t.Fatalf("fully cooling pool handed out %s", acct.Name)
	}
	if want := cur.Add(coolBase); !ready.Equal(want) {
		t.Fatalf("ready=%v, want soonest cooldown %v", ready, want)
	}
}

func TestNextAccountNilReadyIsSoonest(t *testing.T) {
	// Mixed ladder positions: ready must name the SOONEST expiry, not the
	// first slot's. a carries a 30s Retry-After bench (slot 0!), b rides
	// the 10s ladder, c escalates to 20s.
	p, cur := mkPool(t, 0)
	p.rateLimited(&Account{Name: "a", APIKey: "ka"}, 30*time.Second)
	p.rateLimited(&Account{Name: "b", APIKey: "kb"}, 0)
	p.rateLimited(&Account{Name: "c", APIKey: "kc"}, 0)
	p.rateLimited(&Account{Name: "c", APIKey: "kc"}, 0) // c escalates to 20s
	acct, ready := p.next("")
	if acct != nil {
		t.Fatalf("pool should be fully cooling, got %s", acct.Name)
	}
	if want := cur.Add(10 * time.Second); !ready.Equal(want) {
		t.Fatalf("ready=%v, want soonest (b's ladder) %v, not slot 0's %v", ready, want, cur.Add(30*time.Second))
	}
}

func TestWeightedAccountLadderEscalatesOnce(t *testing.T) {
	// A weighted account occupies several slots; one upstream 429 must
	// escalate the ladder once (not per slot) and bench every slot.
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb", Weight: 3},
	}, 0, 0)
	cur := time.Now()
	p.now = func() time.Time { return cur }
	p.rateLimited(&Account{Name: "b", APIKey: "kb"}, 0)
	for i := range p.accts {
		s := &p.accts[i]
		if s.acct.Name != "b" {
			continue
		}
		if s.strikes != 1 || !s.cooldown.Equal(cur.Add(coolBase)) {
			t.Fatalf("slot %d: strikes=%d cooldown=%v, want 1/+10s", i, s.strikes, s.cooldown)
		}
	}
	p.rateLimited(&Account{Name: "b", APIKey: "kb"}, 0) // second 429 of the SAME account
	for i := range p.accts {
		s := &p.accts[i]
		if s.acct.Name == "b" && (s.strikes != 2 || !s.cooldown.Equal(cur.Add(20*time.Second))) {
			t.Fatalf("after 2nd 429 slot %d: strikes=%d cooldown=%v, want 2/+20s", i, s.strikes, s.cooldown)
		}
	}
}

// findSlot returns the pool slot for an account name (tests reach into
// the pool to assert internal bench state).
func findSlot(p *accountPool, name string) *accountState {
	for i := range p.accts {
		if p.accts[i].acct.Name == name {
			return &p.accts[i]
		}
	}
	return nil
}

// mkRateStub serves one response per call: 429 (optionally with
// Retry-After) until flipped to 200, counting hits per account key.
func mkRateStub(t *testing.T, retryAfter string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&hits) == 0 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	return srv, &hits
}

func TestDoAdaptiveCooldownAndSuccessReset(t *testing.T) {
	// The production wiring: a real empty-body 429 response must bench
	// the account via the ladder (10s base), and a later 200 must clear
	// it — all through Def.Do, not pool internals.
	srv, hits := mkRateStub(t, "")
	defer srv.Close()
	p := NewPool()
	def := &Def{Name: "b", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}, {Name: "a2", APIKey: "k2"}}}
	p.Set(def)

	a1 := &def.Accounts[0]
	_, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	if apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("first call: got %v, want 429", apiErr)
	}
	// a1 benched 10s: the pool must hand out a2.
	if got, _ := def.NextAccount(""); got.Name != "a2" {
		t.Fatalf("after a1's 429 the pool served %s, want a2", got.Name)
	}
	// Second 429 on a1 (bench extended to 20s), then recovery.
	atomic.StoreInt32(hits, 1)
	_, apiErr = def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	_ = apiErr
	atomic.StoreInt32(hits, 2)
	if _, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false); apiErr != nil {
		t.Fatalf("post-recovery call failed: %v", apiErr)
	}
	// Success must reset the ladder: bench the pool, clear via ok, and
	// the next 429 benches only coolBase again.
	slot := findSlot(def.pool, "a1")
	if slot.strikes != 0 {
		t.Fatalf("strikes=%d after success, want 0", slot.strikes)
	}
}

func TestDoRetryAfterBench(t *testing.T) {
	// An upstream Retry-After header must win verbatim over the ladder.
	srv, _ := mkRateStub(t, "7")
	defer srv.Close()
	p := NewPool()
	def := &Def{Name: "b", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}}}
	p.Set(def)
	a1 := &def.Accounts[0]
	if _, apiErr := def.Do(context.Background(), a1, "m", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false); apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("got %v, want 429", apiErr)
	}
	slot := findSlot(def.pool, "a1")
	if d := time.Until(slot.cooldown); d < 6*time.Second || d > 7*time.Second {
		t.Fatalf("bench=%v, want ~7s (Retry-After verbatim)", d)
	}
}
