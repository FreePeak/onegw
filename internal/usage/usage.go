// Package usage tracks token consumption with fixed-size lock-sharded
// counters (no per-request map writes) and periodically flushes rollups to
// the store. Memory is O(#shards), independent of request or session count.
package usage

import (
	"sync"
	"sync/atomic"
	"time"

	"onegw/internal/types"
)

const shardCount = 16

// Key identifies a rollup bucket.
type Key struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key"`
	Day      string `json:"day"`  // YYYY-MM-DD (UTC)
	Hour     string `json:"hour"` // HH (UTC) within Day
}

// counters is one shard's fixed-size record.
type counters struct {
	requests     atomic.Int64
	inputTokens  atomic.Int64
	outputTokens atomic.Int64
	cacheRead    atomic.Int64
	cacheWrite   atomic.Int64
	reasoning    atomic.Int64
	savedTokens  atomic.Int64
	estimated    atomic.Int64
	inputSpill   atomic.Int64 // overflow beyond int64 not expected; reserved
	mu           sync.Mutex   // guards migrations only, not hot path
}

// Tracker aggregates usage.
type Tracker struct {
	shards [shardCount]struct {
		mu sync.Mutex
		m  map[Key]*liveBucket
	}
	// Cumulative since process start; never reset by flush.
	totRequests atomic.Int64
	totInput    atomic.Int64
	totOutput   atomic.Int64
	totSaved    atomic.Int64

	stop     chan struct{}
	stopOnce sync.Once
	done     sync.WaitGroup

	sink       Sink
	push       *Pusher
	reset      chan struct{}
	flushEvery atomic.Int64 // nanoseconds; loop re-arms its ticker on reset
}

// Bucket is a flushed rollup.
type Bucket struct {
	Key
	Requests     int64     `json:"requests"`
	InputTokens  int64     `json:"input"`
	OutputTokens int64     `json:"output"`
	CacheRead    int64     `json:"cacheRead"`
	CacheWrite   int64     `json:"cacheWrite"`
	Reasoning    int64     `json:"reasoning"`
	SavedTokens  int64     `json:"saved"`
	Estimated    bool      `json:"estimated"`
	FirstSeen    time.Time `json:"firstSeen"`
	LastSeen     time.Time `json:"lastSeen"`
}

// liveBucket adds mutable timestamps to counters.
type liveBucket struct {
	c         counters
	key       Key
	firstSeen time.Time
	lastSeen  time.Time
}

// Sink receives flushed rollups.
type Sink interface {
	FlushBuckets([]Bucket) error
}

// New starts a tracker flushing every interval to sink (nil sink = memory
// only; flushEvery <= 0 resets to the 30 s default). push (may be nil) gets
// every flushed window after the sink write — the optional remote export.
func New(sink Sink, flushEvery time.Duration, push *Pusher) *Tracker {
	if flushEvery <= 0 {
		flushEvery = 30 * time.Second
	}
	t := &Tracker{
		stop:  make(chan struct{}),
		reset: make(chan struct{}, 1),
		sink:  sink,
		push:  push,
	}
	t.flushEvery.Store(int64(flushEvery))
	for i := range t.shards {
		t.shards[i].m = make(map[Key]*liveBucket)
	}
	t.done.Add(1)
	go t.loop()
	return t
}

func (t *Tracker) shardFor(k Key) *struct {
	mu sync.Mutex
	m  map[Key]*liveBucket
} {
	h := fnv32(k.Provider) ^ fnv32(k.Model) ^ fnv32(k.APIKey)
	return &t.shards[int(h)%shardCount]
}

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	if s == "" {
		return 1
	}
	return h
}

// Observe records one completed request.
func (t *Tracker) Observe(k Key, u types.Usage, savedTokens int64) {
	if k.Day == "" {
		now := time.Now().UTC()
		k.Day = now.Format("2006-01-02")
		k.Hour = now.Format("15")
	}
	sh := t.shardFor(k)
	sh.mu.Lock()
	b, ok := sh.m[k]
	if !ok {
		b = &liveBucket{key: k, firstSeen: time.Now()}
		sh.m[k] = b
	}
	b.lastSeen = time.Now()
	sh.mu.Unlock()
	t.totRequests.Add(1)
	t.totInput.Add(u.InputTokens)
	t.totOutput.Add(u.OutputTokens)
	if savedTokens > 0 {
		t.totSaved.Add(savedTokens)
	}
	b.c.requests.Add(1)
	b.c.inputTokens.Add(u.InputTokens)
	b.c.outputTokens.Add(u.OutputTokens)
	b.c.cacheRead.Add(u.CacheReadTokens)
	b.c.cacheWrite.Add(u.CacheWriteTokens)
	b.c.reasoning.Add(u.ReasoningTokens)
	if savedTokens > 0 {
		b.c.savedTokens.Add(savedTokens)
	}
	if u.Estimated {
		b.c.estimated.Add(1)
	}
}

// Totals returns cumulative counters since process start.
func (t *Tracker) Totals() (requests, input, output, saved int64) {
	return t.totRequests.Load(), t.totInput.Load(), t.totOutput.Load(), t.totSaved.Load()
}

// SetFlushInterval changes the flush cadence without dropping accumulated
// data: the loop goroutine survives and picks up the new ticker after its
// current tick. d <= 0 resets to the 30 s default.
//
// Safe to call at any time, including after Stop: the notify is a
// non-blocking send, so a stopped tracker (or a loop currently mid-flush)
// can never wedge the caller — the next loop iteration just reads the
// freshest interval. Callers racing Stop lose only the final reset.
func (t *Tracker) SetFlushInterval(d time.Duration) {
	if d <= 0 {
		d = 30 * time.Second
	}
	t.flushEvery.Store(int64(d))
	select {
	case t.reset <- struct{}{}:
	default:
	}
}

// loop flushes periodically and honors SetFlushInterval resets.
func (t *Tracker) loop() {
	defer t.done.Done()
	d := time.Duration(t.flushEvery.Load())
	if d <= 0 {
		d = 30 * time.Second
	}
	tick := time.NewTicker(d)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			t.flushOnce()
			return
		case <-tick.C:
			t.flushOnce()
		case <-t.reset:
			tick.Reset(time.Duration(t.flushEvery.Load()))
		}
	}
}

func (t *Tracker) flushOnce() {
	var out []Bucket
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for k, b := range sh.m {
			out = append(out, Bucket{
				Key:          k,
				Requests:     b.c.requests.Load(),
				InputTokens:  b.c.inputTokens.Load(),
				OutputTokens: b.c.outputTokens.Load(),
				CacheRead:    b.c.cacheRead.Load(),
				CacheWrite:   b.c.cacheWrite.Load(),
				Reasoning:    b.c.reasoning.Load(),
				SavedTokens:  b.c.savedTokens.Load(),
				Estimated:    b.c.estimated.Load() > 0,
				FirstSeen:    b.firstSeen,
				LastSeen:     b.lastSeen,
			})
			// Reset counters; keep the bucket (zeroed) so map stays bounded
			// by distinct keys, not request volume.
			b.c.requests.Store(0)
			b.c.inputTokens.Store(0)
			b.c.outputTokens.Store(0)
			b.c.cacheRead.Store(0)
			b.c.cacheWrite.Store(0)
			b.c.reasoning.Store(0)
			b.c.savedTokens.Store(0)
			b.c.estimated.Store(0)
		}
		sh.mu.Unlock()
	}
	if t.sink != nil && len(out) > 0 {
		_ = t.sink.FlushBuckets(out)
	}
	if len(out) > 0 {
		t.push.AfterFlush(out) // fire-and-forget; never blocks this loop
	}
}

// Stop flushes remaining data and ends the loop. Idempotent.
func (t *Tracker) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
	t.done.Wait()
}

// Snapshot returns current in-memory totals (for admin endpoints).
func (t *Tracker) Snapshot() []Bucket {
	var out []Bucket
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for k, b := range sh.m {
			out = append(out, Bucket{
				Key:          k,
				Requests:     b.c.requests.Load(),
				InputTokens:  b.c.inputTokens.Load(),
				OutputTokens: b.c.outputTokens.Load(),
				CacheRead:    b.c.cacheRead.Load(),
				CacheWrite:   b.c.cacheWrite.Load(),
				Reasoning:    b.c.reasoning.Load(),
				SavedTokens:  b.c.savedTokens.Load(),
				Estimated:    b.c.estimated.Load() > 0,
				FirstSeen:    b.firstSeen,
				LastSeen:     b.lastSeen,
			})
		}
		sh.mu.Unlock()
	}
	return out
}
