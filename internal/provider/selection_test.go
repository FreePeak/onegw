package provider

import (
	"testing"
	"time"
)

// selPool builds a 3-account pool with a deterministic pickN source.
func selPool(t *testing.T, mode string, headroom func(string) (float64, bool)) *Def {
	t.Helper()
	d := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{
		{Name: "a", APIKey: "ka"}, {Name: "b", APIKey: "kb"}, {Name: "c", APIKey: "kc"},
	}}
	d.pool = newAccountPool(d.Accounts, 0, 0)
	d.pool.pickN = func(n int) int { return 0 } // deterministic: "first" sample
	d.SetSelection(mode, headroom)
	return d
}

// The default mode must keep the shipped behavior: fastest open slot,
// round-robin order among equal speeds (a fresh pool has no samples).
func TestSelectionDefaultKeepsRoundRobin(t *testing.T) {
	d := selPool(t, "", nil)
	var got []string
	for i := 0; i < 6; i++ {
		a, _ := d.NextAccount("")
		if a == nil {
			t.Fatalf("round %d: pool empty", i)
		}
		got = append(got, a.Name)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default selection changed: got %v want %v", got, want)
		}
	}
}

// least-used: after a,b,c have each served once, the oldest-served leads.
func TestSelectionLeastUsed(t *testing.T) {
	d := selPool(t, "least-used", nil)
	for i := 0; i < 3; i++ {
		if a, _ := d.NextAccount(""); a == nil {
			t.Fatal("pool empty")
		}
	}
	// Backdate b so least-used must prefer it (a and c are more recent).
	d.pool.mu.Lock()
	for i := range d.pool.accts {
		if d.pool.accts[i].acct.Name == "b" {
			d.pool.accts[i].lastUsed = time.Now().Add(-time.Hour)
		}
	}
	d.pool.mu.Unlock()
	a, _ := d.NextAccount("")
	if a == nil || a.Name != "b" {
		t.Fatalf("least-used must pick the oldest-served account, got %v", a)
	}
}

// strict-random: a shuffled deck serves every account once before repeating.
func TestSelectionStrictRandomDeck(t *testing.T) {
	d := selPool(t, "strict-random", nil)
	counts := map[string]int{}
	var order []string
	for i := 0; i < 3; i++ {
		a, _ := d.NextAccount("")
		if a == nil {
			t.Fatal("pool empty")
		}
		counts[a.Name]++
		order = append(order, a.Name)
	}
	if len(counts) != 3 {
		t.Fatalf("deck must serve each account once before repeating: %v", order)
	}
}

// p2c: with two open slots the lower score wins; subscription headroom is
// part of that score, so a nearly-spent key loses to a fresh one even when
// its cooldown gates are open.
func TestSelectionP2CPrefersHeadroom(t *testing.T) {
	head := func(acct string) (float64, bool) {
		if acct == "a" {
			return 5, true // nearly spent
		}
		if acct == "b" {
			return 95, true // fresh
		}
		return 0, false
	}
	d := selPool(t, "p2c", head)
	// Age everything so recency does not dominate, and give both the same
	// (unknown) speed: headroom is then the deciding signal.
	d.pool.mu.Lock()
	for i := range d.pool.accts {
		d.pool.accts[i].lastUsed = time.Now().Add(-time.Hour)
	}
	d.pool.mu.Unlock()

	first, _ := d.NextAccount("")
	if first == nil || first.Name == "a" {
		t.Fatalf("p2c must avoid the nearly-spent account, got %v", first)
	}

	// Strikes also count: b recently 429ed twice, a is clean and fresh.
	d2 := selPool(t, "p2c", nil)
	d2.pool.mu.Lock()
	for i := range d2.pool.accts {
		s := &d2.pool.accts[i]
		s.lastUsed = time.Now().Add(-time.Hour)
		if s.acct.Name == "b" {
			s.strikes = 4
		}
	}
	d2.pool.mu.Unlock()
	got, _ := d2.NextAccount("")
	if got == nil || got.Name == "b" {
		t.Fatalf("p2c must avoid the recently rate-limited account, got %v", got)
	}
}

// A blocked slot is never selected by any strategy, and a single open slot is
// returned regardless of mode.
func TestSelectionRespectsGates(t *testing.T) {
	for _, mode := range []string{"", "p2c", "least-used", "strict-random", "random"} {
		d := selPool(t, mode, nil)
		d.pool.mu.Lock()
		for i := range d.pool.accts {
			if d.pool.accts[i].acct.Name != "c" {
				d.pool.accts[i].cooldown = time.Now().Add(time.Hour)
			}
		}
		d.pool.mu.Unlock()
		for i := 0; i < 3; i++ {
			a, _ := d.NextAccount("")
			if a == nil || a.Name != "c" {
				t.Fatalf("mode %q: must pick the only open slot, got %v", mode, a)
			}
		}
	}
}

// Union guard with the peer's in-flight pick: a slot with a live upstream
// call must lose to an idle sibling in EVERY selection mode — the
// per-key-concurrency incident fix (3 stacked 330K prefills → TTFB
// 29s/32s/170s) cannot be undone by opting into a strategy.
func TestSelectionOccupancyGateAcrossModes(t *testing.T) {
	for _, mode := range []string{"", "p2c", "least-used", "strict-random", "random"} {
		d := selPool(t, mode, nil)
		d.pool.mu.Lock()
		now := d.pool.now()
		for i := range d.pool.accts {
			s := &d.pool.accts[i]
			s.lastUsed = now.Add(-time.Hour)
			// Busy: "a" has one call in flight. Idle: b and c.
			if s.acct.Name == "a" {
				s.live = 1
			}
			// Make "b" the LESS attractive normal candidate (slow, struck),
			// so a mode that ignored occupancy would happily pick "a" or
			// "b"; only the occupancy gate makes "c" correct... but any idle
			// slot is acceptable, so assert the pick is not the busy one.
			if s.acct.Name == "b" {
				s.strikes = 5
			}
		}
		d.pool.mu.Unlock()
		for i := 0; i < 3; i++ {
			a, _ := d.NextAccount("")
			if a == nil {
				t.Fatalf("mode %q: pool empty", mode)
			}
			if a.Name == "a" {
				t.Fatalf("mode %q: picked the slot with a call in flight (occupancy gate lost)", mode)
			}
		}
	}
}
