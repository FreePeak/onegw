package usage

import (
	"sync"
	"testing"
	"time"

	"onegw/internal/types"
)

type sinkStub struct {
	mu      sync.Mutex
	flushes [][]Bucket
}

func (s *sinkStub) FlushBuckets(b []Bucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := append([]Bucket(nil), b...)
	s.flushes = append(s.flushes, cp)
	return nil
}

func (s *sinkStub) totalRequests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, f := range s.flushes {
		for _, b := range f {
			n += b.Requests
		}
	}
	return n
}

func (s *sinkStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flushes)
}

func (s *sinkStub) firstLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flushes[0])
}

func TestSetFlushIntervalResetsCadence(t *testing.T) {
	sink := &sinkStub{}
	tr := New(sink, time.Hour, nil) // effectively never flushes on its own
	defer tr.Stop()

	tr.Observe(Key{Provider: "p", Model: "m"}, types.Usage{InputTokens: 1, OutputTokens: 2}, 0)
	tr.SetFlushInterval(20 * time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	flushes := 0
	for flushes == 0 {
		flushes = sink.count()
		if time.Now().After(deadline) {
			t.Fatal("flush interval change did not take effect within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := sink.firstLen(); got != 1 {
		t.Fatalf("first flush should carry the observed bucket, got %d buckets", got)
	}
}

func TestSetFlushIntervalPreservesData(t *testing.T) {
	sink := &sinkStub{}
	tr := New(sink, time.Hour, nil)
	defer tr.Stop()

	tr.Observe(Key{Provider: "p", Model: "m"}, types.Usage{InputTokens: 3, OutputTokens: 4}, 0)
	tr.SetFlushInterval(15 * time.Millisecond)
	// Observe again after the reset: nothing may be lost or double-counted.
	tr.Observe(Key{Provider: "p", Model: "m"}, types.Usage{InputTokens: 1, OutputTokens: 1}, 0)

	deadline := time.Now().Add(2 * time.Second)
	var flushed int64
	var count int64
	for time.Now().Before(deadline) {
		flushed, count = 0, 0
		sink.mu.Lock()
		for _, f := range sink.flushes {
			for _, b := range f {
				flushed += b.InputTokens + b.OutputTokens
				count += b.Requests
			}
		}
		sink.mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count != 2 {
		t.Fatalf("expected both observations to reach the sink, got %d requests", count)
	}
	if want := int64(3 + 4 + 1 + 1); flushed != want {
		t.Fatalf("token totals = %d, want %d (no loss on interval change)", flushed, want)
	}
}

func TestSetFlushIntervalZeroResetsDefault(t *testing.T) {
	tr := New(nil, time.Hour, nil)
	defer tr.Stop()
	tr.SetFlushInterval(0)
	if d := time.Duration(tr.flushEvery.Load()); d != 30*time.Second {
		t.Fatalf("zero interval should reset to 30s, got %v", d)
	}
}

// Regression: Stop must not wedge later SetFlushInterval callers — the
// notify send is non-blocking. Before the fix, a second call after Stop
// blocked forever (buffer full, loop goroutine gone).
func TestSetFlushIntervalAfterStopDoesNotWedge(t *testing.T) {
	tr := New(nil, time.Hour, nil)
	tr.SetFlushInterval(5 * time.Millisecond) // absorbed by the buffer
	tr.Stop()

	done := make(chan struct{})
	go func() {
		tr.SetFlushInterval(10 * time.Millisecond) // buffer full, loop gone
		tr.SetFlushInterval(20 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SetFlushInterval blocked after Stop — reset send is not non-blocking")
	}
}

// Hot reload calls SetFlushInterval from the signal goroutine while the
// loop may be mid-flush; concurrent calls must all return.
func TestSetFlushIntervalConcurrent(t *testing.T) {
	sink := &sinkStub{}
	tr := New(sink, 10*time.Millisecond, nil)
	tr.Observe(Key{Provider: "p", Model: "m"}, types.Usage{InputTokens: 1}, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			tr.SetFlushInterval(10 * time.Millisecond)
			tr.SetFlushInterval(25 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent SetFlushInterval calls blocked")
	}
	tr.Stop()
}
