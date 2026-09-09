package provider

import (
	"testing"
	"time"
)

// mkGovPool builds a pool from accounts with a controllable clock.
func mkGovPool(t *testing.T, accts []Account) (*accountPool, *time.Time) {
	t.Helper()
	p := newAccountPool(accts, 0, 0)
	cur := time.Now()
	p.now = func() time.Time { return cur }
	return p, &cur
}

// A governed account must stop receiving picks once its bucket drains —
// before the upstream per-account 429 benches it — while uncapped siblings
// keep serving. This is the behavior the b-ai error storm demanded: the
// pool rotates proactively instead of burning doomed attempts.
func TestRPMGovernorRotatesBeforeUpstreamLimit(t *testing.T) {
	p, cur := mkGovPool(t, []Account{
		{Name: "governed", APIKey: "kg", RPM: 1},
		{Name: "free", APIKey: "kf"},
	})
	seen := map[string]int{}
	for range 4 {
		a, ready := p.next("")
		if a == nil {
			t.Fatalf("pick %d: pool empty, ready=%v", len(seen), ready)
		}
		seen[a.Name]++
		*cur = cur.Add(time.Second)
	}
	if seen["governed"] != 2 { // burst capacity 2, then drained (rpm=1 → 1 token/60s, ~4s elapsed refills nothing)
		t.Fatalf("governed picks = %d, want 2 (burst cap)", seen["governed"])
	}
	if seen["free"] != 2 {
		t.Fatalf("free picks = %d, want 2", seen["free"])
	}
}

// Refill must re-enable the account: rpm=6 → one token per 10s.
func TestRPMRefillReEnables(t *testing.T) {
	p, cur := mkGovPool(t, []Account{{Name: "a", APIKey: "ka", RPM: 6}})
	for range 2 { // burst
		if a, _ := p.next(""); a == nil {
			t.Fatal("burst pick blocked")
		}
	}
	_, ready := p.next("")
	if ready.IsZero() || ready.Before(cur.Add(9*time.Second)) || ready.After(cur.Add(11*time.Second)) {
		t.Fatalf("ready = %v, want ~10s out", ready)
	}
	*cur = ready // advance to refill instant
	if a, _ := p.next(""); a == nil {
		t.Fatal("pick after refill blocked")
	}
}

// A fully governed pool reports an honest soonest-ready instead of a doomed
// pick (pool-empty path feeds the client Retry-After).
func TestRPMPoolEmptyHonestReady(t *testing.T) {
	p, cur := mkGovPool(t, []Account{{Name: "a", APIKey: "ka", RPM: 60}})
	for range 2 {
		p.next("")
	}
	_, ready := p.next("")
	if ready.IsZero() || !ready.After(*cur) {
		t.Fatalf("ready = %v, want future refill instant", ready)
	}
}

// Regression guard (hint-loss refactor): cooldown-only blocks must still
// surface the ladder bench as the pool-empty ready time.
func TestCooldownOnlyPoolStillHintsLadderBench(t *testing.T) {
	p, cur := mkGovPool(t, []Account{{Name: "a", APIKey: "ka"}})
	p.rateLimited(&Account{Name: "a", APIKey: "ka"}, 0)
	_, ready := p.next("")
	if ready.IsZero() || !ready.After(cur.Add(5*time.Second)) {
		t.Fatalf("ready = %v, want ~coolBase(10s) bench", ready)
	}
}

// Weighted slots of one governed account share a single bucket.
func TestRPMWeightedSlotsShareBucket(t *testing.T) {
	p, _ := mkGovPool(t, []Account{
		{Name: "w", APIKey: "kw", Weight: 3, RPM: 1},
		{Name: "f", APIKey: "kf"},
	})
	gov := 0
	for range 6 {
		a, _ := p.next("")
		if a == nil {
			t.Fatal("pool empty")
		}
		if a.Name == "w" {
			gov++
		}
	}
	if gov != 2 {
		t.Fatalf("weighted governed picks = %d, want 2 (burst cap on ONE shared bucket)", gov)
	}
}

// rpm=0 stays uncapped — existing pools must be behaviorally unchanged.
func TestRPMZeroUncapped(t *testing.T) {
	p, _ := mkGovPool(t, []Account{{Name: "a", APIKey: "ka"}})
	for range 50 {
		if a, _ := p.next(""); a == nil {
			t.Fatal("uncapped account drained")
		}
	}
}

// Sticky affinity must break (and rotate) when the pinned account's bucket
// is drained — a governed pin must not starve the session.
func TestRPMStickyPinRotatesWhenGoverned(t *testing.T) {
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka", RPM: 1},
		{Name: "b", APIKey: "kb"},
	}, time.Hour, 0)
	cur := time.Now()
	p.now = func() time.Time { return cur }
	first, _ := p.next("sess")
	if first.Name != "a" {
		t.Fatalf("first pick = %s, want a", first.Name)
	}
	second, _ := p.next("sess") // pin reuse: burst token 2, still a
	if second.Name != "a" {
		t.Fatalf("pinned pick = %s, want a", second.Name)
	}
	cur = cur.Add(time.Second)
	third, _ := p.next("sess") // bucket drained → rotate past the pin
	if third.Name != "b" {
		t.Fatalf("post-drain pick = %s, want b (pin must rotate)", third.Name)
	}
}
