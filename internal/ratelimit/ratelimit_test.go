package ratelimit

import (
	"sync"
	"testing"
)

// withClock injects a controllable unix-second clock.
func withClock(t *testing.T, start int64) (*Limiter, *int64) {
	t.Helper()
	now := start
	l := New()
	l.now = func() int64 { return now }
	return l, &now
}

func TestAllowRPMBurstDeny(t *testing.T) {
	l, _ := withClock(t, 1_000_000)
	for i := 0; i < 5; i++ {
		if ok, _ := l.AllowRPM("k", 5); !ok {
			t.Fatalf("request %d denied before limit", i+1)
		}
	}
	ok, retry := l.AllowRPM("k", 5)
	if ok {
		t.Fatal("6th request allowed past rpm=5")
	}
	if retry < 1 || retry > 60 {
		t.Fatalf("retry = %d, want 1..60", retry)
	}
}

func TestAllowRPMSlidingWindowRefill(t *testing.T) {
	l, now := withClock(t, 1_000_000)
	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowRPM("k", 3); !ok {
			t.Fatalf("request %d denied", i+1)
		}
	}
	// Advance 61s: every bucket has aged out; the window is empty again.
	*now += windowSeconds + 1
	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowRPM("k", 3); !ok {
			t.Fatalf("request %d denied after window slide", i+1)
		}
	}
}

func TestAllowRPMRetryMatchesOldestBucket(t *testing.T) {
	l, now := withClock(t, 1_000_000)
	if ok, _ := l.AllowRPM("k", 1); !ok {
		t.Fatal("first request denied")
	}
	// The sole bucket was stamped at t=1_000_000; the window frees at
	// t+60, so retry now (t=1_000_000) must be exactly 60.
	ok, retry := l.AllowRPM("k", 1)
	if ok || retry != 60 {
		t.Fatalf("ok=%v retry=%d, want false/60", ok, retry)
	}
	// Partially aged: same bucket at t+59 still needs 1 more second.
	*now += 59
	ok, retry = l.AllowRPM("k", 1)
	if ok || retry != 1 {
		t.Fatalf("ok=%v retry=%d, want false/1", ok, retry)
	}
	// Fully aged: allowed again.
	*now++
	if ok, _ := l.AllowRPM("k", 1); !ok {
		t.Fatal("request denied after bucket aged out")
	}
}

func TestAllowRPMZeroMeansUnlimited(t *testing.T) {
	l, _ := withClock(t, 1_000_000)
	for i := 0; i < 10_000; i++ {
		if ok, _ := l.AllowRPM("k", 0); !ok {
			t.Fatal("rpm=0 denied a request")
		}
	}
	l.mu.Lock()
	_, hasEntry := l.m["k"]
	l.mu.Unlock()
	if hasEntry {
		t.Fatal("unlimited key created a limiter entry")
	}
}

func TestTPMObserveAndBlock(t *testing.T) {
	l, now := withClock(t, 2_000_000)
	// No entry yet: not blocked.
	if blocked, _ := l.TPMBlocked("k", 1000); blocked {
		t.Fatal("blocked with no observed tokens")
	}
	l.ObserveTPM("k", 1000, 600)
	if blocked, _ := l.TPMBlocked("k", 1000); blocked {
		t.Fatal("blocked below tpm")
	}
	l.ObserveTPM("k", 1000, 500)
	blocked, retry := l.TPMBlocked("k", 1000)
	if !blocked || retry < 1 || retry > 60 {
		t.Fatalf("blocked=%v retry=%d, want true/1..60", blocked, retry)
	}
	// Tokens age out like requests do.
	*now += windowSeconds + 1
	if blocked, _ := l.TPMBlocked("k", 1000); blocked {
		t.Fatal("still blocked after window slid")
	}
}

func TestTPMZeroMeansUnlimited(t *testing.T) {
	l, _ := withClock(t, 2_000_000)
	l.ObserveTPM("k", 0, 1<<40)
	if blocked, _ := l.TPMBlocked("k", 0); blocked {
		t.Fatal("tpm=0 blocked")
	}
	l.mu.Lock()
	_, hasEntry := l.m["k"]
	l.mu.Unlock()
	if hasEntry {
		t.Fatal("tpm=0 key created a limiter entry")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := withClock(t, 3_000_000)
	if ok, _ := l.AllowRPM("a", 1); !ok {
		t.Fatal("key a denied on first use")
	}
	if ok, _ := l.AllowRPM("b", 1); !ok {
		t.Fatal("key b denied although key a exhausted its window")
	}
	if ok, _ := l.AllowRPM("a", 1); ok {
		t.Fatal("key a allowed twice within window")
	}
	l.ObserveTPM("a", 100, 100)
	if blocked, _ := l.TPMBlocked("b", 100); blocked {
		t.Fatal("key b blocked by key a's tokens")
	}
}

func TestConcurrentUse(t *testing.T) {
	l, _ := withClock(t, 4_000_000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := string(rune('a' + g))
			for i := 0; i < 200; i++ {
				l.AllowRPM(key, 100)
				l.ObserveTPM(key, 1000, 10)
				l.TPMBlocked(key, 1000)
			}
		}(g)
	}
	wg.Wait()
}
