package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ByteBudget bounds the memory held by buffered (non-streaming) work: request
// body reads, saver passes, unified decode, non-stream translation.
// Streaming passthrough never acquires it.
//
// Implementation: mutex + coarse poll (2 ms). A token channel cannot express
// all-or-nothing multi-unit take without deadlock (partial holders starve
// each other), so the budget is a single integer guarded by a lock.
type ByteBudget struct {
	mu        sync.Mutex
	held      int64
	capacity  int64
	waiting   atomic.Int64
	exhausted atomic.Int64
}

const budgetPollInterval = 2 * time.Millisecond

var errTooLarge = errors.New("request exceeds budget capacity")

// NewByteBudget creates a budget of capBytes (minimum 1 KiB).
func NewByteBudget(capBytes int64) *ByteBudget {
	if capBytes < 1024 {
		capBytes = 1024
	}
	return &ByteBudget{capacity: capBytes}
}

// Acquire takes n bytes atomically, waiting (polling) until available or ctx
// is done. Requests larger than the whole capacity fail immediately.
func (b *ByteBudget) Acquire(ctx context.Context, n int64) error {
	if n > b.capacity {
		// Clamp oversized requests to the full capacity instead of failing:
		// a single request may exceed the budget if misconfigured; gate it at
		// the max rather than rejecting forever.
		n = b.capacity
	}
	b.waiting.Add(1)
	defer b.waiting.Add(-1)

	b.mu.Lock()
	if b.capacity-b.held >= n {
		b.held += n
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	tick := time.NewTicker(budgetPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			b.exhausted.Add(1)
			return ctx.Err()
		case <-tick.C:
		}
		b.mu.Lock()
		if b.capacity-b.held >= n {
			b.held += n
			b.mu.Unlock()
			return nil
		}
		b.mu.Unlock()
	}
}

// Release returns n bytes to the budget.
func (b *ByteBudget) Release(n int64) {
	b.mu.Lock()
	b.held -= n
	if b.held < 0 {
		b.held = 0
	}
	b.mu.Unlock()
}

// Saturated records a rejected request for visibility.
func (b *ByteBudget) Saturated() { b.exhausted.Add(1) }

// Stats reports budget pressure: current held bytes and rejected count.
func (b *ByteBudget) Stats() (heldBytes, rejected int64) {
	b.mu.Lock()
	h := b.held
	b.mu.Unlock()
	return h, b.exhausted.Load()
}

// Waiting reports the number of acquirers currently blocked.
func (b *ByteBudget) Waiting() int64 { return b.waiting.Load() }
