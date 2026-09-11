package provider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// b-ai 2026-09-11 (seqs 879-898, then the live repro): three identical
// 330K-token prefills issued at once to ONE key serialize upstream — TTFB
// 29s / 32s / 170s — because the vendor's free keys allow ~1 concurrent
// request. The deepest-queued attempt rides past the gateway's 75s
// pre-first-byte budget and 504s while the key is healthy and the sibling
// keys sit idle. Selection therefore has to spread concurrent attempts
// across the pool; decode speed stays the tiebreak so idle traffic still
// prefers the quicker credential.

// seedSpeed folds one decode sample into a slot so the speed tiebreak is
// deterministic (the pool is otherwise all zero-data, i.e. round-robin).
func seedSpeed(p *accountPool, idx int, tps float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accts[idx].speed.observe(int64(tps)*4, time.Second, time.Now())
}

// liveOf reports a slot's occupancy without racing the pick loop.
func liveOf(p *accountPool, name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accts {
		if p.accts[i].acct.Name == name {
			return p.accts[i].live
		}
	}
	return -1
}

func TestPickSpreadsOffBusyAccount(t *testing.T) {
	_, def := newDef(t, "http://127.0.0.1:1") // never dialed: selection only
	p := def.pool
	fast, slow := &def.Accounts[0], &def.Accounts[1]
	seedSpeed(p, 0, 400) // clone2 decodes faster: it wins an idle pick

	if a, _ := p.next(""); a == nil || a.Name != fast.Name {
		t.Fatalf("idle pick must honor speed, got %+v", a)
	}

	// One attempt in flight on the fast key: the next pick must spread to
	// the idle sibling instead of stacking a second prefill behind it.
	if n := p.begin(fast); n != 1 {
		t.Fatalf("begin: want depth 1, got %d", n)
	}
	a, _ := p.next("")
	if a == nil || a.Name != slow.Name {
		t.Fatalf("in-flight fast key must yield to an idle sibling, got %+v", a)
	}

	// Released: the fast key wins again (occupancy is not a bench).
	p.end(fast)
	if got := liveOf(p, fast.Name); got != 0 {
		t.Fatalf("end must release the slot, live=%d", got)
	}
	if a, _ := p.next(""); a == nil || a.Name != fast.Name {
		t.Fatalf("released key must win again, got %+v", a)
	}
}

func TestDoHoldsOccupancyUntilReturn(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the attempt in the pre-first-byte phase
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)

	_, def := newDef(t, srv.URL)
	p := def.pool
	fast, slow := &def.Accounts[0], &def.Accounts[1]
	seedSpeed(p, 0, 400)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = def.Do(context.Background(), fast, "glm-5.3-flash", nil,
			bytes.NewReader([]byte(`{"model":"glm-5.3-flash","messages":[]}`)), false)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for liveOf(p, fast.Name) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("Do never registered its in-flight attempt")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if a, _ := p.next(""); a == nil || a.Name != slow.Name {
		t.Fatalf("while Do waits on headers the slot is busy; got %+v", a)
	}

	close(release)
	<-done
	if got := liveOf(p, fast.Name); got != 0 {
		t.Fatalf("Do must release its slot on return, live=%d", got)
	}
}
