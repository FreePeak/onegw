// Package ratelimit implements per-key request and token rate limiting.
//
// Each limit is a sliding 60-second window approximated by 60 one-second
// buckets: counting into the current second, summing the window. Buckets
// age out lazily during the sum, so there is no background sweeper and no
// allocation on the hot path.
//
// Entries are created only for keys that actually carry a policy; keys
// with unlimited rpm/tpm never touch the map.
package ratelimit

import "sync"

const windowSeconds = 60

// bucket counts events observed during one wall-clock second.
type bucket struct {
	sec int64 // unix second this bucket covers
	v   int64
}

// entry holds the two windows for one key.
type entry struct {
	req [windowSeconds]bucket
	tok [windowSeconds]bucket
}

// Limiter tracks per-key sliding windows. Safe for concurrent use.
type Limiter struct {
	mu  sync.Mutex
	m   map[string]*entry
	now func() int64 // unix seconds; injectable for tests
}

// New returns a ready limiter.
func New() *Limiter {
	return &Limiter{m: map[string]*entry{}, now: defaultNow}
}

func defaultNow() int64 { return unixNow() }

// get returns (creating if needed) the entry for key.
func (l *Limiter) get(key string) *entry {
	e, ok := l.m[key]
	if !ok {
		e = &entry{}
		l.m[key] = e
	}
	return e
}

// sumAt sums a window, zeroing buckets that have aged out of it. Returns
// the sum and the unix second at which the oldest contributing bucket
// leaves the window (0 if nothing contributes).
func sumAt(w *[windowSeconds]bucket, now int64) (int64, int64) {
	var sum int64
	oldest := int64(0)
	for i := range w {
		b := &w[i]
		if b.v == 0 {
			continue
		}
		if now-b.sec >= windowSeconds {
			b.v = 0 // stale: reset in place, no sweeper needed
			continue
		}
		sum += b.v
		if oldest == 0 || b.sec < oldest {
			oldest = b.sec
		}
	}
	return sum, oldest
}

// add increments the bucket covering now.
func add(w *[windowSeconds]bucket, now int64, v int64) {
	i := int(now % windowSeconds)
	if w[i].sec != now {
		w[i].sec = now
		w[i].v = 0
	}
	w[i].v += v
}

// AllowRPM records one request for key if its 60s request window stays
// under rpm (rpm <= 0 means unlimited and always allows without touching
// the map). ok=false comes with retry: the seconds until the oldest
// contributing request leaves the window, minimum 1.
func (l *Limiter) AllowRPM(key string, rpm int) (ok bool, retry int) {
	if rpm <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.get(key)
	sum, oldest := sumAt(&e.req, now)
	if sum >= int64(rpm) {
		if oldest == 0 {
			oldest = now
		}
		return false, maxInt(1, int(oldest+windowSeconds-now))
	}
	add(&e.req, now, 1)
	return true, 0
}

// TPMBlocked reports whether key's observed token usage in the last 60s
// has reached tpm (tpm <= 0 means unlimited). Retry counts the seconds
// until the oldest contributing bucket leaves the window.
func (l *Limiter) TPMBlocked(key string, tpm int) (blocked bool, retry int) {
	if tpm <= 0 {
		return false, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[key]
	if !ok {
		return false, 0
	}
	sum, oldest := sumAt(&e.tok, now)
	if sum < int64(tpm) {
		return false, 0
	}
	if oldest == 0 {
		oldest = now
	}
	return true, maxInt(1, int(oldest+windowSeconds-now))
}

// ObserveTPM records completed tokens for key. No-op when tpm <= 0: keys
// without a token policy never create an entry.
func (l *Limiter) ObserveTPM(key string, tpm int, tokens int64) {
	if tpm <= 0 || tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	add(&l.get(key).tok, l.now(), tokens)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
