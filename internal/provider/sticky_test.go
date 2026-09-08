package provider

import (
	"testing"
	"time"
)

// mkPool builds a 3-account pool (each weight 1 → one slot per account)
// with the given sticky TTL and a controllable clock.
func mkPool(t *testing.T, sticky time.Duration) (*accountPool, *time.Time) {
	t.Helper()
	p := newAccountPool([]Account{
		{Name: "a", APIKey: "ka"},
		{Name: "b", APIKey: "kb"},
		{Name: "c", APIKey: "kc"},
	}, sticky)
	cur := time.Now()
	p.now = func() time.Time { return cur }
	return p, &cur
}

func TestPlainRoundRobinUnchangedWithoutSticky(t *testing.T) {
	p, _ := mkPool(t, 0)
	// No identity: plain RR regardless of ttl.
	var names []string
	for range 3 {
		names = append(names, p.next("").Name)
	}
	if names[0] == names[1] || names[1] == names[2] {
		t.Fatalf("plain RR should rotate: %v", names)
	}
	// With ttl=0 even a non-empty identity stays plain RR.
	if p.next("sess").Name == p.next("sess").Name {
		t.Fatal("ttl=0 must ignore identity")
	}
}

func TestStickyPinsIdentityAcrossCalls(t *testing.T) {
	p, _ := mkPool(t, 5*time.Minute)
	first := p.next("sess")
	for range 4 {
		if got := p.next("sess"); got != first {
			t.Fatalf("pin drifted: got %s want %s", got.Name, first.Name)
		}
	}
	// A different identity gets its own pin (not serialized onto a's key).
	second := p.next("other")
	if second.Name == first.Name {
		t.Fatalf("distinct identity should rotate to next account, got %s twice", first.Name)
	}
	if got := p.next("other"); got != second {
		t.Fatalf("second identity pin drifted: %s vs %s", got.Name, second.Name)
	}
}

func TestStickyExpiresAfterTTL(t *testing.T) {
	p, cur := mkPool(t, 5*time.Minute)
	first := p.next("sess")
	*cur = cur.Add(6 * time.Minute)
	if got := p.next("sess"); got == first {
		t.Fatal("expired pin must rotate to the next account")
	}
}

func TestStickyRotatesWhenPinnedAccountCools(t *testing.T) {
	p, cur := mkPool(t, 5*time.Minute)
	first := p.next("sess")
	p.cool(first, time.Minute) // pinned key exhausted upstream
	*cur = cur.Add(time.Second)
	got := p.next("sess")
	if got.Name == first.Name {
		t.Fatal("cooling pinned account must rotate")
	}
	// The rotated pick re-pins: subsequent calls follow the new account.
	if again := p.next("sess"); again != got {
		t.Fatalf("re-pin drifted: %s vs %s", again.Name, got.Name)
	}
}

func TestUnpinFreesTheIdentity(t *testing.T) {
	p, _ := mkPool(t, 5*time.Minute)
	first := p.next("sess")
	p.unpin("sess")
	if got := p.next("sess"); got == first {
		t.Fatal("after unpin the next pick must rotate to a different account")
	}
}

func TestFailedAttemptNeverRePinsSameAccount(t *testing.T) {
	// Router-level contract: NextAccount(id) then Unpin(id) on failure
	// means the retry (and combo fallthrough) lands on a different key.
	p, _ := mkPool(t, 5*time.Minute)
	failed := p.next("sess")
	p.unpin("sess")
	retry := p.next("sess")
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
	}, 5*time.Minute)
	cur := time.Now()
	p.now = func() time.Time { return cur }

	pinned := p.next("sess")
	p.unpin("sess")
	got := p.next("sess")
	if got.Name == pinned.Name {
		t.Fatal("rotate after unpin must move accounts")
	}
}

func TestStickyIdentityEmptyFallsBackToRR(t *testing.T) {
	p, _ := mkPool(t, 5*time.Minute)
	a := p.next("")
	b := p.next("")
	if a.Name == b.Name {
		t.Fatal("empty identity must stay plain round-robin")
	}
}
