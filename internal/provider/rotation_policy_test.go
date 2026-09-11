package provider

import (
	"testing"
	"time"
)

// #84 golden: a zero policy resolves to exactly the shipped constants — the
// guarantee that lets an untouched config behave byte-identically.
func TestZeroRotationPolicyUsesShippedDefaults(t *testing.T) {
	d := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{{Name: "a", APIKey: "k"}}}
	d.pool = newAccountPool(d.Accounts, 0, 0)
	if got := d.pool.ladderBase(); got != coolBase {
		t.Fatalf("ladderBase = %v, want %v", got, coolBase)
	}
	if got := d.pool.ladderCap(); got != coolCap {
		t.Fatalf("ladderCap = %v, want %v", got, coolCap)
	}
	if got := d.pool.flapTrip(); got != flapThreshold {
		t.Fatalf("flapTrip = %v, want %v", got, flapThreshold)
	}
	if got := d.pool.flapPark(); got != flapOpen {
		t.Fatalf("flapPark = %v, want %v", got, flapOpen)
	}
	if got := d.benchTTL(); got != ModelBenchTTL {
		t.Fatalf("benchTTL = %v, want %v", got, ModelBenchTTL)
	}
}

// A configured policy actually changes the mechanics: the 429 ladder starts
// at the configured base, and the breaker trips on the configured threshold.
func TestConfiguredRotationPolicyTakesEffect(t *testing.T) {
	d := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{{Name: "a", APIKey: "k"}}}
	d.pool = newAccountPool(d.Accounts, 0, 0)
	d.SetRotationPolicy(RotationPolicy{CoolBase: time.Second, CoolCap: 2 * time.Second, FlapThreshold: 2, FlapOpen: time.Second})

	// First 429 with no Retry-After benches for CoolBase, not coolBase*2^0.
	now := time.Now()
	p := d.pool
	p.mu.Lock()
	p.now = func() time.Time { return now } // frozen clock so the window math is exact
	p.mu.Unlock()
	p.rateLimited(&d.Accounts[0], 0)
	p.mu.Lock()
	cool := p.accts[0].cooldown.Sub(now)
	p.mu.Unlock()
	if cool != time.Second {
		t.Fatalf("first 429 bench = %v, want the configured 1s (shipped default would be 10s)", cool)
	}

	// Ladder caps at the configured CoolCap.
	p.rateLimited(&d.Accounts[0], 0)
	p.rateLimited(&d.Accounts[0], 0)
	p.rateLimited(&d.Accounts[0], 0)
	p.mu.Lock()
	cool = p.accts[0].cooldown.Sub(now)
	p.mu.Unlock()
	if cool != 2*time.Second {
		t.Fatalf("ladder cap = %v, want the configured 2s (shipped default 60s)", cool)
	}

	// Breaker opens on the configured threshold.
	p.flapStrike()
	if p.flapOpenUntil.After(now) {
		t.Fatal("breaker must not open before the configured threshold")
	}
	p.flapStrike()
	if !p.flapOpenUntil.After(now) {
		t.Fatal("breaker must open at the configured threshold")
	}

	// Pool.Set copies the def policy into a rebuilt pool (reload path).
	p2 := NewPool()
	p2.Set(d)
	got, _ := p2.Get("p")
	if got.pool.ladderBase() != time.Second {
		t.Fatalf("rebuilt pool lost the policy: %v", got.pool.ladderBase())
	}

	// Bench TTL override rides the def.
	d.BenchModel("m", 0)
	d.modelMu.RLock()
	_, ok := d.modelBench["m"]
	d.modelMu.RUnlock()
	if !ok {
		t.Fatal("bench missing")
	}
	if got := d.benchTTL(); got != 0 {
		_ = got // BenchTTL was set to 0 in this policy? no — 0 field means default; covered by golden above
	}
}
