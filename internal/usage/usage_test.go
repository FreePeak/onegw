package usage

import (
	"sync"
	"testing"
	"time"

	"onegw/internal/types"
)

// memSink is called from the tracker's background loop goroutine, so it
// must be safe for concurrent use like the real SQLite sink.
type memSink struct {
	mu      sync.Mutex
	buckets []Bucket
}

func (m *memSink) FlushBuckets(bs []Bucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets = append(m.buckets, bs...)
	return nil
}

func (m *memSink) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

func (m *memSink) snapshot() []Bucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Bucket(nil), m.buckets...)
}

func (m *memSink) clear() {
	m.mu.Lock()
	m.buckets = nil
	m.mu.Unlock()
}

func TestObserveAndFlush(t *testing.T) {
	sink := &memSink{}
	tr := New(sink, 30*time.Millisecond)
	defer tr.Stop()

	k := Key{Provider: "p", Model: "m", APIKey: "k"}
	for range 5 {
		tr.Observe(k, types.Usage{InputTokens: 10, OutputTokens: 4}, 100)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.len() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := sink.len(); got != 1 {
		t.Fatalf("want 1 bucket, got %d", got)
	}
	b := sink.snapshot()[0]
	if b.Requests != 5 || b.InputTokens != 50 || b.OutputTokens != 20 || b.SavedTokens != 500 {
		t.Fatalf("bad rollup: %+v", b)
	}
	// After flush, counters reset; new observe starts fresh.
	sink.clear()
	tr.Observe(k, types.Usage{InputTokens: 1, OutputTokens: 1}, 0)
	tr.Stop() // final flush
	final := sink.snapshot()
	if len(final) != 1 || final[0].Requests != 1 {
		t.Fatalf("post-reset flush wrong: %+v", final)
	}
}

func TestShardingDistinctKeys(t *testing.T) {
	tr := New(nil, time.Hour)
	defer tr.Stop()
	for i := range 1000 {
		tr.Observe(Key{Provider: "p", Model: "m", APIKey: string(rune('a' + i%26))},
			types.Usage{InputTokens: 1, OutputTokens: 1}, 0)
	}
	snap := tr.Snapshot()
	if len(snap) != 26 {
		t.Fatalf("want 26 buckets, got %d", len(snap))
	}
}

func TestEmptyKeyFNVStable(t *testing.T) {
	if fnv32("") == 0 {
		t.Fatal("fnv32 empty")
	}
}
