package idempotency

import (
	"context"
	"testing"
	"time"
)

// newTestCache builds a cache with a controllable clock so TTL tests
// never sleep.
func newTestCache(t *testing.T, capacity int, ttl time.Duration) (*Cache, *time.Time) {
	t.Helper()
	c := New(capacity, ttl)
	if c == nil {
		t.Fatal("New returned nil")
	}
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	return c, &now
}

// claimFinish claims a free pair and settles it with res, asserting the
// claim was a Miss.
func claimFinish(t *testing.T, c *Cache, scope, key string, hash []byte, res *Result) {
	t.Helper()
	if d, _, _ := c.Claim(scope, key, hash); d != Miss {
		t.Fatalf("claim %s: got %v, want Miss", key, d)
	}
	c.Finish(scope, key, hash, res)
}

func TestLRUEvictionOrder(t *testing.T) {
	c, _ := newTestCache(t, 3, time.Minute)
	for _, k := range []string{"a", "b", "c"} {
		claimFinish(t, c, "s", k, BodyHash([]byte(k)), &Result{Status: 200, Body: []byte(k)})
	}
	// Touch "a": it must become the most recently used.
	if d, got, _ := c.Claim("s", "a", BodyHash([]byte("a"))); d != Replay || string(got.Body) != "a" {
		t.Fatalf("touch a: got %v %v, want Replay a", d, got)
	}
	// One over capacity: "b" (least recently used) must go, not "a"/"c".
	claimFinish(t, c, "s", "d", BodyHash([]byte("d")), &Result{Status: 200, Body: []byte("d")})
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	if d, _, _ := c.Claim("s", "b", BodyHash([]byte("b"))); d != Miss {
		t.Fatal("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if d, _, _ := c.Claim("s", k, BodyHash([]byte(k))); d != Replay {
			t.Fatalf("%s survived, want Replay", k)
		}
	}
}

func TestTTLExpiry(t *testing.T) {
	c, now := newTestCache(t, 8, 5*time.Second)
	claimFinish(t, c, "s", "k", BodyHash([]byte("b")), &Result{Status: 200, Body: []byte("ok")})
	if d, _, _ := c.Claim("s", "k", BodyHash([]byte("b"))); d != Replay {
		t.Fatal("within TTL: want Replay")
	}
	*now = now.Add(6 * time.Second)
	if d, _, _ := c.Claim("s", "k", BodyHash([]byte("b"))); d != Miss {
		t.Fatal("past TTL: want Miss")
	}
	if c.Len() != 1 {
		// The expired entry was replaced by the fresh in-flight one.
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

func TestCoalesceWaitsForOriginal(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	hash := BodyHash([]byte("body"))
	if d, _, _ := c.Claim("s", "k", hash); d != Miss {
		t.Fatal("first claim: want Miss")
	}
	d2, _, w := c.Claim("s", "k", hash)
	if d2 != Wait || w == nil {
		t.Fatalf("second claim: got %v, want Wait", d2)
	}
	c.Finish("s", "k", hash, &Result{Status: 200, ContentType: "application/json", Body: []byte("same")})
	got := w.Wait(context.Background())
	if got == nil || got.Gone || got.Stream || string(got.Body) != "same" || got.Status != 200 {
		t.Fatalf("waiter got %+v, want the original's result", got)
	}
}

func TestWaiterCtxDeadline(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	hash := BodyHash([]byte("body"))
	if d, _, _ := c.Claim("s", "k", hash); d != Miss {
		t.Fatal("first claim: want Miss")
	}
	_, _, w := c.Claim("s", "k", hash)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := w.Wait(ctx); got != nil {
		t.Fatalf("expired wait returned %+v, want nil", got)
	}
	// The origin still settles normally afterwards.
	c.Finish("s", "k", hash, &Result{Status: 200, Body: []byte("late")})
	if d, _, _ := c.Claim("s", "k", hash); d != Replay {
		t.Fatal("origin result should still be replayable")
	}
}

func TestReplayNonStreamResult(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	hash := BodyHash([]byte("body"))
	want := &Result{Status: 201, ContentType: "application/json", Body: []byte(`{"ok":true}`)}
	claimFinish(t, c, "s", "k", hash, want)
	d, got, _ := c.Claim("s", "k", hash)
	if d != Replay {
		t.Fatalf("got %v, want Replay", d)
	}
	if got.Status != want.Status || got.ContentType != want.ContentType || string(got.Body) != string(want.Body) || got.Stream {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// The replay is a copy: mutating it must not corrupt the entry.
	got.Body[0] = 'X'
	if d, again, _ := c.Claim("s", "k", hash); d != Replay || again.Body[0] != '{' {
		t.Fatal("replayed body must be a defensive copy")
	}
}

func TestStreamShapeConflicts(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	hash := BodyHash([]byte("body"))
	claimFinish(t, c, "s", "k", hash, &Result{Status: 200, Stream: true})
	if d, _, _ := c.Claim("s", "k", hash); d != Conflict {
		t.Fatalf("stream result: got %v, want Conflict", d)
	}

	// A non-stream body that overflows EntryBodyCap is recorded
	// stream-shaped too (no replay).
	big := make([]byte, EntryBodyCap+1)
	claimFinish(t, c, "s", "big", BodyHash([]byte("big")), &Result{Status: 200, Body: big})
	if d, _, _ := c.Claim("s", "big", BodyHash([]byte("big"))); d != Conflict {
		t.Fatalf("oversize body: got %v, want Conflict", d)
	}
}

func TestCapBound(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	for i := 0; i < 16; i++ {
		k := string(rune('a' + i))
		claimFinish(t, c, "s", k, BodyHash([]byte(k)), &Result{Status: 200, Body: []byte{byte(i)}})
	}
	if c.Len() > 8 {
		t.Fatalf("Len = %d after filling 2x capacity, want <= 8", c.Len())
	}
}

func TestGlobalBodyBudget(t *testing.T) {
	c, _ := newTestCache(t, MaxEntriesHard, time.Minute)
	body := make([]byte, EntryBodyCap) // 1 MiB each; 5 bust the 4 MiB budget
	for i := 0; i < 5; i++ {
		k := string(rune('a' + i))
		claimFinish(t, c, "s", k, BodyHash([]byte(k)), &Result{Status: 200, Body: body})
	}
	if c.bytes > TotalBodyBudget {
		t.Fatalf("bytes = %d, want <= %d", c.bytes, TotalBodyBudget)
	}
	if c.Len() >= 5 {
		t.Fatalf("Len = %d, entries must have been evicted to fit the budget", c.Len())
	}
}

func TestGoneWakesWaiters(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	hash := BodyHash([]byte("body"))
	if d, _, _ := c.Claim("s", "k", hash); d != Miss {
		t.Fatal("first claim: want Miss")
	}
	_, _, w := c.Claim("s", "k", hash)
	c.Finish("s", "k", hash, nil) // origin wrote nothing
	if got := w.Wait(context.Background()); got == nil || !got.Gone {
		t.Fatalf("waiter got %+v, want Gone", got)
	}
	// The vanished origin's key is free again: a fresh claim executes.
	if d, _, _ := c.Claim("s", "k", hash); d != Miss {
		t.Fatalf("after Gone: got %v, want Miss", d)
	}
}

func TestScopeAndBodyIsolation(t *testing.T) {
	c, _ := newTestCache(t, 8, time.Minute)
	hash := BodyHash([]byte("body"))
	claimFinish(t, c, "s1", "k", hash, &Result{Status: 200, Body: []byte("one")})
	// Different credential scope: separate execution, no cross-client replay.
	if d, _, _ := c.Claim("s2", "k", hash); d != Miss {
		t.Fatal("another client's same key must not replay")
	}
	// Different body hash: a distinct pair.
	if d, _, _ := c.Claim("s1", "k", BodyHash([]byte("other"))); d != Miss {
		t.Fatal("same key with a different body must not replay")
	}
	if d, _, _ := c.Claim("s1", "k", hash); d != Replay {
		t.Fatal("same scope+key+body must replay")
	}
}

func TestNewDisablesOnNonPositiveTTL(t *testing.T) {
	if c := New(8, 0); c != nil {
		t.Fatal("ttl 0 must disable the cache (nil)")
	}
	if c := New(8, -time.Second); c != nil {
		t.Fatal("negative ttl must disable the cache (nil)")
	}
	if c := New(0, time.Second); c == nil || c.cap != DefaultEntries {
		t.Fatal("capacity 0 must select the default")
	}
	if c := New(MaxEntriesHard+1, time.Second); c == nil || c.cap != MaxEntriesHard {
		t.Fatal("capacity past the hard max must clamp")
	}
}
