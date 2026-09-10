package provider

import (
	"testing"
	"time"
)

func TestSpeedSampleObserve(t *testing.T) {
	now := time.Unix(1789060000, 0)
	var s speedSample

	// Tiny/short samples are noise: a 3-token 20ms reply must not move it.
	s.observe(3, 20*time.Millisecond, now)
	s.observe(500, 100*time.Millisecond, now)
	if s.n != 0 || s.tps() != 0 {
		t.Fatalf("noise samples must be ignored: n=%d tps=%f", s.n, s.tps())
	}

	// First real sample seeds directly: 400 tokens over 4s = 100 tok/s.
	s.observe(400, 4*time.Second, now)
	if s.n != 1 || s.tps() != 100 {
		t.Fatalf("first sample must seed: n=%d tps=%f", s.n, s.tps())
	}

	// Second sample folds in at 25%: 50 tok/s → 87.5.
	s.observe(100, 2*time.Second, now)
	if s.tps() != 87.5 {
		t.Fatalf("EWMA fold wrong: %f, want 87.5", s.tps())
	}

	// A sample after staleAfter resets instead of averaging across the gap.
	s.observe(400, 4*time.Second, now.Add(staleAfter+time.Minute))
	if s.n != 3 || s.tps() != 100 {
		t.Fatalf("stale gap must reset: n=%d tps=%f", s.n, s.tps())
	}
}

func newSpeedTestPool(t *testing.T, names []string) *accountPool {
	t.Helper()
	accts := make([]Account, len(names))
	for i, n := range names {
		accts[i] = Account{Name: n, APIKey: "key-" + n}
	}
	return newAccountPool(accts, 0, 0)
}

func TestNextAccountPrefersFastest(t *testing.T) {
	p := newSpeedTestPool(t, []string{"slow", "fast", "idle"})
	now := time.Unix(1789060000, 0)
	p.now = func() time.Time { return now }

	// Seed: slow = 50 tok/s, fast = 200 tok/s, idle = no data.
	p.observeSpeed(&Account{Name: "slow", APIKey: "key-slow"}, 500, 10*time.Second, now)
	p.observeSpeed(&Account{Name: "fast", APIKey: "key-fast"}, 400, 2*time.Second, now)

	// Round-robin pointer at "slow" (index 0): the old first-open pick
	// would serve slow; speed steering must skip to fast.
	a, ready := p.next("")
	if a == nil || a.Name != "fast" {
		t.Fatalf("got %v ready=%v, want fast", a, ready)
	}

	// Equal/no-data speeds keep round-robin order (fair rotation).
	p2 := newSpeedTestPool(t, []string{"a", "b"})
	p2.now = func() time.Time { return now }
	a1, _ := p2.next("")
	a2, _ := p2.next("")
	a3, _ := p2.next("")
	if a1.Name != "a" || a2.Name != "b" || a3.Name != "a" {
		t.Fatalf("no-data pool must round-robin: got %s, %s, %s", a1.Name, a2.Name, a3.Name)
	}
}

func TestNextAccountFastestSkipsCooling(t *testing.T) {
	p := newSpeedTestPool(t, []string{"fast", "slow"})
	now := time.Unix(1789060000, 0)
	p.now = func() time.Time { return now }

	p.observeSpeed(&Account{Name: "fast", APIKey: "key-fast"}, 400, 2*time.Second, now)
	// Bench the fast account: the slower one must take over.
	p.cool(&Account{Name: "fast", APIKey: "key-fast"}, 30*time.Second)
	a, ready := p.next("")
	if a == nil || a.Name != "slow" {
		t.Fatalf("got %v ready=%v, want slow", a, ready)
	}
}

func TestSpeedRowsSnapshot(t *testing.T) {
	p := newSpeedTestPool(t, []string{"x", "y", "z"})
	now := time.Unix(1789060000, 0)
	p.observeSpeed(&Account{Name: "x", APIKey: "key-x"}, 100, 10*time.Second, now) // 10 tok/s
	p.observeSpeed(&Account{Name: "y", APIKey: "key-y"}, 400, 2*time.Second, now)  // 200 tok/s

	rows := p.speedRows()
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Account != "y" || rows[0].TPS != 200 || rows[1].Account != "x" {
		t.Fatalf("rows not fastest-first: %+v", rows)
	}
}
