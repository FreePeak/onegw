package usage

import (
	"testing"
	"time"

	"onegw/internal/types"
)

type memSink struct{ buckets []Bucket }

func (m *memSink) FlushBuckets(bs []Bucket) error {
	m.buckets = append(m.buckets, bs...)
	return nil
}

func TestObserveAndFlush(t *testing.T) {
	sink := &memSink{}
	tr := New(sink, 30*time.Millisecond)
	defer tr.Stop()

	k := Key{Provider: "p", Model: "m", APIKey: "k"}
	for i := 0; i < 5; i++ {
		tr.Observe(k, types.Usage{InputTokens: 10, OutputTokens: 4}, 100)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.buckets) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(sink.buckets) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(sink.buckets))
	}
	b := sink.buckets[0]
	if b.Requests != 5 || b.InputTokens != 50 || b.OutputTokens != 20 || b.SavedTokens != 500 {
		t.Fatalf("bad rollup: %+v", b)
	}
	// After flush, counters reset; new observe starts fresh.
	sink.buckets = nil
	tr.Observe(k, types.Usage{InputTokens: 1, OutputTokens: 1}, 0)
	tr.Stop() // final flush
	if len(sink.buckets) != 1 || sink.buckets[0].Requests != 1 {
		t.Fatalf("post-reset flush wrong: %+v", sink.buckets)
	}
}

func TestShardingDistinctKeys(t *testing.T) {
	tr := New(nil, time.Hour)
	defer tr.Stop()
	for i := 0; i < 1000; i++ {
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
		t.Fatal("empty string hash collides with shard 0 requirement")
	}
}
