package provider

import (
	"testing"
	"time"
)

func mkRunPool(t *testing.T, minRun, maxRun int, accts ...Account) (*accountPool, *time.Time, func() string) {
	t.Helper()
	p := newAccountPool(accts, time.Hour, 0)
	var cur time.Time
	cur = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return cur }
	p.policy = RotationPolicy{AccountRunMin: minRun, AccountRunMax: maxRun}
	p.pickN = func(int) int { return 0 }
	pick := func() string {
		a, _ := p.next("")
		if a ***REMOVED*** nil {
			t.Fatalf("pool unexpectedly cooling")
		}
		return a.Name
	}
	return p, &cur, pick
}

func TestAccountRunKeepsAccountForWholeRun(t *testing.T) {
	_, _, pick := mkRunPool(t, 20, 50,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
		Account{Name: "c", APIKey: "kc"},
	)
	want := []string{"a", "b", "c", "a", "b", "c"}
	for round, name := range want {
		for i := range 20 {
			if got := pick(); got != name {
				t.Fatalf("round %d request %d: got %s, want %s (run must not rotate early)", round, i, got, name)
			}
		}
	}
}

func TestAccountRunDrawsFreshLengthPerRun(t *testing.T) {
	p, _, pick := mkRunPool(t, 20, 50,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
	)
	draws := []int{19, 29}
	i := 0
	p.pickN = func(n int) int {
		if n != 31 {
			t.Fatalf("draw range = %d, want 31 values for 20-50", n)
		}
		v := draws[i%len(draws)]
		i++
		return v
	}
	for req := range 39 {
		if got := pick(); got != "a" {
			t.Fatalf("request %d: got %s, want a (run 1 length 39)", req, got)
		}
	}
	for req := range 49 {
		if got := pick(); got != "b" {
			t.Fatalf("request %d: got %s, want b (run 2 length 49)", req, got)
		}
	}
}

func TestAccountRunEndsOnFailure(t *testing.T) {
	p, _, pick := mkRunPool(t, 20, 50,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
	)
	for range 10 {
		if got := pick(); got != "a" {
			t.Fatalf("got %s want a", got)
		}
	}
	p.endRunFor(&Account{Name: "a", APIKey: "ka"})
	if got := pick(); got != "b" {
		t.Fatalf("after a's failure, got %s want b", got)
	}
	for range 19 {
		if got := pick(); got != "b" {
			t.Fatalf("got %s want b", got)
		}
	}
	if got := pick(); got != "a" {
		t.Fatalf("after b's run, got %s want a", got)
	}
}

func TestAccountRunSuccessDoesNotEndRun(t *testing.T) {
	_, _, pick := mkRunPool(t, 20, 50,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
	)
	for range 20 { pick() }
	for range 20 {
		if got := pick(); got != "b" {
			t.Fatalf("got %s want b after run completes", got)
		}
	}
}

func TestAccountRunOffKeepsPickOrderRotation(t *testing.T) {
	p, _, pick := mkRunPool(t, 0, 0,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
	)
	want := []string{"a", "b", "a", "b", "a"}
	for i, w := range want {
		if got := pick(); got != w {
			t.Fatalf("request %d: got %s want %s", i, got, w)
		}
	}
	if p.runs != -1 {
		t.Fatalf("run flag off but runs=%d", p.runs)
	}
}

func TestAccountRunFailureOnSiblingDoesNotEndRun(t *testing.T) {
	p, _, pick := mkRunPool(t, 20, 50,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
	)
	for range 5 { pick() }
	p.endRunFor(&Account{Name: "b", APIKey: "kb"})
	for range 15 {
		if got := pick(); got != "a" {
			t.Fatalf("got %s want a (sibling failure should not end run)", got)
		}
	}
}

func TestAccountRunEndsWhenItsAccountCools(t *testing.T) {
	p, _, pick := mkRunPool(t, 20, 50,
		Account{Name: "a", APIKey: "ka"},
		Account{Name: "b", APIKey: "kb"},
	)
	for range 5 { pick() }
	p.mu.Lock()
	p.accts[0].cooldown = p.now().Add(time.Hour)
	p.mu.Unlock()
	if got := pick(); got != "b" {
		t.Fatalf("got %s want b (a cooling must end run)", got)
	}
}
