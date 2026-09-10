// Package idempotency deduplicates retried gateway requests that carry an
// Idempotency-Key (or X-Request-Id fallback) header. Coding agents retry
// flaky requests; within a short TTL an identical retry must not
// double-execute upstream (it would double-burn combo attempts and can
// land on a different provider than the original).
//
// Semantics: a (scope, key, body-hash) pair seen within TTL either
// coalesces onto the still-in-flight original (waiter receives the same
// result), replays the recorded non-streaming response, or — for a
// stream-shaped original — answers 409 so the client can retry with a
// fresh key. Streaming bodies are never recorded: that would violate the
// gateway's RAM budget (GOMEMLIMIT=90MiB).
//
// The cache is a bounded LRU with a hard entry cap and a global body-byte
// budget. Expired entries are pruned lazily on lookup and eviction; there
// is no background goroutine and no timer.
package idempotency

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// MaxEntriesHard bounds the entry count regardless of configuration.
	MaxEntriesHard = 1024

	// DefaultEntries is the LRU capacity when the config leaves it unset.
	DefaultEntries = 128

	// EntryBodyCap is the largest buffered response body per entry; a
	// response that grows past it (or flushes mid-body) marks the entry
	// stream-shaped and it will never be replayed.
	EntryBodyCap = 1 << 20

	// TotalBodyBudget bounds the sum of all buffered bodies across the
	// cache: worst case 4 MiB of bodies plus per-entry overhead, well
	// inside the 90 MiB GOMEMLIMIT even at the 1024-entry clamp.
	TotalBodyBudget = 4 << 20
)

// Decision tells the middleware what to do with a Claim.
type Decision int

const (
	// Miss: no entry — the caller is the origin of record and must call
	// Finish exactly once with the outcome.
	Miss Decision = iota
	// Replay: a finished non-streaming result is available.
	Replay
	// Conflict: the recorded result was stream-shaped (no replay).
	Conflict
	// Wait: the origin is still in flight; block on the waiter.
	Wait
)

// Result is one settled request outcome.
type Result struct {
	Status      int
	ContentType string
	Body        []byte
	// Stream marks a stream-shaped outcome: replay answers 409.
	Stream bool
	// Gone marks a vanished origin (handler wrote nothing): waiters
	// re-claim and execute themselves instead of coalescing onto
	// nothing.
	Gone bool
}

func (r *Result) clone() *Result {
	if r == nil {
		return nil
	}
	c := *r
	if r.Body != nil {
		c.Body = append([]byte(nil), r.Body...)
	}
	return &c
}

// ScopeFor derives the per-client cache scope from the raw credential
// (same extraction order as the server's authorize): requests
// authenticated with different keys never share entries, while the same
// credential retrying with the same key does. A fixed-size hash — not the
// raw key — keeps secrets out of memory dumps; the empty credential (open
// gateway) is a valid scope.
func ScopeFor(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:8])
}

// BodyHash is the request-body fingerprint bound into the cache key.
func BodyHash(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

// compositeKey binds the idempotency key to the credential scope and the
// body hash. The same key with a different body is a distinct pair and
// executes separately (the contract scopes dedup by (key, body-hash)).
func compositeKey(scope, key string, bodyHash []byte) string {
	h := sha256.New()
	h.Write([]byte(scope))
	h.Write([]byte{0})
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write(bodyHash)
	return hex.EncodeToString(h.Sum(nil))
}

type entry struct {
	key      string // composite key; map/list identity
	created  time.Time
	complete bool
	res      *Result
	waiters  []*waiter
}

// waiter receives the settled result of the request it coalesced with.
type waiter struct {
	ch chan *Result
}

// Wait blocks until the original request settles or ctx is done (the
// wait is capped at the caller's remaining context deadline). nil means
// ctx expired first: the caller proceeds unrecorded — an upstream call on
// a dead context fails fast anyway.
func (w *waiter) Wait(ctx context.Context) *Result {
	select {
	case res := <-w.ch:
		return res
	case <-ctx.Done():
		return nil
	}
}

// Cache is the bounded LRU of settled request results. Safe for
// concurrent use; a single mutex is deliberate — contention is trivially
// low (one map+list touch per participating request).
type Cache struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	now   func() time.Time // injectable for tests
	ll    *list.List       // front = most recently used
	items map[string]*list.Element
	bytes int // sum of recorded non-stream bodies
}

// New builds the cache. capacity 0 selects DefaultEntries and values past
// MaxEntriesHard are clamped. ttl <= 0 disables the whole feature (nil
// cache); callers must nil-check.
func New(capacity int, ttl time.Duration) *Cache {
	if ttl <= 0 {
		return nil
	}
	if capacity <= 0 {
		capacity = DefaultEntries
	}
	if capacity > MaxEntriesHard {
		capacity = MaxEntriesHard
	}
	return &Cache{
		cap:   capacity,
		ttl:   ttl,
		now:   time.Now,
		ll:    list.New(),
		items: make(map[string]*list.Element, capacity),
	}
}

// Claim registers interest in (scope, key, bodyHash). See Decision for
// the outcomes; the returned waiter is non-nil only for Wait.
func (c *Cache) Claim(scope, key string, bodyHash []byte) (Decision, *Result, *waiter) {
	ck := compositeKey(scope, key, bodyHash)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[ck]; ok {
		e := el.Value.(*entry)
		switch {
		case c.expired(e):
			// TTL passed: the pair is no longer "seen within TTL".
			// An expired origin loses its record (its Finish no-ops)
			// and its waiters are woken with Gone.
			c.remove(el)
		case !e.complete:
			w := &waiter{ch: make(chan *Result, 1)}
			e.waiters = append(e.waiters, w)
			return Wait, nil, w
		default:
			c.ll.MoveToFront(el)
			if e.res.Stream {
				return Conflict, nil, nil
			}
			return Replay, e.res.clone(), nil
		}
	}
	e := &entry{key: ck, created: c.now()}
	c.items[ck] = c.ll.PushFront(e)
	return Miss, nil, nil
}

// Finish settles the origin's request. A nil result means the handler
// wrote nothing (pre-response failure or panic): the entry is dropped and
// waiters re-claim and execute themselves. Otherwise the result is stored
// (stream-shaped entries carry no body) and fanned out to waiters.
func (c *Cache) Finish(scope, key string, bodyHash []byte, res *Result) {
	ck := compositeKey(scope, key, bodyHash)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[ck]
	if !ok {
		return // evicted or TTL-dropped while in flight
	}
	e := el.Value.(*entry)
	if e.complete {
		return // defensive: Finish is documented as exactly-once
	}
	if res == nil || res.Gone || res.Status == 0 {
		c.remove(el)
		return
	}
	if res.Stream || len(res.Body) > EntryBodyCap {
		res = &Result{Status: res.Status, Stream: true}
	}
	e.complete = true
	e.res = res
	if !res.Stream {
		c.bytes += len(res.Body)
	}
	c.wake(e, res)
	c.enforce()
}

// enforce keeps the LRU under the entry cap and the body pool under the
// global byte budget, evicting least-recently-used entries first.
func (c *Cache) enforce() {
	for len(c.items) > c.cap || c.bytes > TotalBodyBudget {
		victim := c.evictable()
		if victim == nil {
			return // everything left is in flight; caps are transiently exceeded
		}
		c.remove(victim)
	}
}

// evictable scans back-to-front for the LRU entry that may be dropped:
// complete, or expired while still in flight. In-flight unexpired entries
// are never evicted — losing one would strand its waiters — so with every
// entry in flight the caps may be exceeded (bounded by actual concurrency).
func (c *Cache) evictable() *list.Element {
	for el := c.ll.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*entry)
		if e.complete || c.expired(e) {
			return el
		}
	}
	return nil
}

// remove drops an entry; an incomplete one wakes its waiters with the
// Gone sentinel so they re-claim and serve themselves.
func (c *Cache) remove(el *list.Element) {
	e := el.Value.(*entry)
	c.wake(e, &Result{Gone: true})
	if e.complete && e.res != nil && !e.res.Stream {
		c.bytes -= len(e.res.Body)
	}
	delete(c.items, e.key)
	c.ll.Remove(el)
}

func (c *Cache) wake(e *entry, res *Result) {
	for _, w := range e.waiters {
		w.ch <- res.clone() // buffered cap 1, exactly one send per waiter
	}
	e.waiters = nil
}

func (c *Cache) expired(e *entry) bool {
	return c.now().Sub(e.created) > c.ttl
}

// Len reports the number of live entries (tests and metrics).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
