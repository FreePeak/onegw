package provider

import (
	"testing"
	"time"
)

// TestInvalidateRemovesAccountFromRotation pins the terminal contract (#80):
// an invalidated slot is never offered, no timer brings it back, and a success
// on a sibling does not clear it.
func TestInvalidateRemovesAccountFromRotation(t *testing.T) {
	def := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{
		{Name: "a", APIKey: "ka"}, {Name: "b", APIKey: "kb"},
	}}
	def.pool = newAccountPool(def.Accounts, 0, 0)

	a := &def.Accounts[0]
	if !def.Invalidate(a) {
		t.Fatal("first invalidate must report a fresh transition")
	}
	if def.Invalidate(a) {
		t.Fatal("second invalidate must not report a fresh transition (log-once contract)")
	}

	// Every pick over many rounds must land on b; a success elsewhere must
	// not resurrect a.
	for i := 0; i < 6; i++ {
		got, _ := def.NextAccount("")
		if got == nil {
			t.Fatalf("round %d: pool reports empty while b is healthy", i)
		}
		if got.Name != "b" {
			t.Fatalf("round %d: picked %q, want b (a is terminal)", i, got.Name)
		}
		def.pool.ok(got, time.Now().Add(-time.Second))
	}
	if names := def.Invalidated(); len(names) != 1 || names[0] != "a" {
		t.Fatalf("Invalidated() = %v, want [a]", names)
	}
	if def.AllInvalidated() {
		t.Fatal("AllInvalidated must be false while b can serve")
	}

	// Operator reset restores rotation.
	if !def.Revalidate(a) {
		t.Fatal("Revalidate must report clearing a terminal account")
	}
	if def.Revalidate(a) {
		t.Fatal("second Revalidate must be a no-op")
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got, _ := def.NextAccount("")
		seen[got.Name] = true
	}
	if !seen["a"] {
		t.Fatal("after Revalidate, a must rejoin rotation")
	}
}

// TestAllInvalidatedReportsUnfundedPool: one terminal account and no siblings
// is the whole-billing-dead case the server answers with a 503 (not a rate
// limit), so the predicate must be exact about "every account".
func TestAllInvalidatedReportsUnfundedPool(t *testing.T) {
	def := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{{Name: "only", APIKey: "k"}}}
	def.pool = newAccountPool(def.Accounts, 0, 0)
	if def.AllInvalidated() {
		t.Fatal("a healthy single-account pool must not report unfunded")
	}
	def.Invalidate(&def.Accounts[0])
	if !def.AllInvalidated() {
		t.Fatal("invalidating the only account must report the pool unfunded")
	}
	if got, _ := def.NextAccount(""); got != nil {
		t.Fatalf("terminal account must not be offered, got %+v", got)
	}
	// A terminal slot is not a cooldown: no instant makes it ready.
	if ok, ready := def.pool.availableForTest(0); ok || !ready.IsZero() {
		t.Fatalf("terminal slot must never become ready, got ok=%v ready=%v", ok, ready)
	}
}

// TestCarryInvalidatedSurvivesReloadButNotKeyRotation: a reload must not
// resurrect a refused key, and a rotated credential must.
func TestCarryInvalidatedSurvivesReloadButNotKeyRotation(t *testing.T) {
	build := func(key string) *Pool {
		d := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{{Name: "a", APIKey: key}}}
		d.pool = newAccountPool(d.Accounts, 0, 0)
		p := NewPool()
		p.Set(d)
		return p
	}

	old := build("ka")
	oldDef, _ := old.Get("p")
	oldDef.Invalidate(&oldDef.Accounts[0])

	// Same credential: the vendor still refuses it, so the fresh pool keeps
	// the terminal state (no doomed first request after every SIGHUP).
	fresh := build("ka")
	CarryInvalidated(old, fresh)
	fd, _ := fresh.Get("p")
	if !fd.AllInvalidated() {
		t.Fatal("reload with the same key must keep the terminal state")
	}

	// Rotated credential under the same account name: fresh start.
	rotated := build("kb")
	CarryInvalidated(old, rotated)
	rd, _ := rotated.Get("p")
	if rd.AllInvalidated() {
		t.Fatal("a rotated api_key must start active")
	}
	if got, _ := rd.NextAccount(""); got == nil {
		t.Fatal("rotated credential must be offered")
	}
}

// availableForTest exposes slot readiness for the index given (test helper
// kept in the test file so the production API stays minimal).
func (p *accountPool) availableForTest(i int) (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.available(&p.accts[i], time.Now())
}
