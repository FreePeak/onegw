package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestByteBudgetBoundsConcurrentAcquisitions(t *testing.T) {
	b := NewByteBudget(16 * 1024) // 16 KiB total

	const workers = 32
	var peak atomic.Int64
	var workersWG sync.WaitGroup
	stop := make(chan struct{})

	// Sample live held bytes while work is in flight.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				if held, _ := b.Stats(); held > peak.Load() {
					peak.Store(held)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			ctx := context.Background()
			if err := b.Acquire(ctx, 4*1024); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(2 * time.Millisecond)
			b.Release(4 * 1024)
		}()
	}
	workersWG.Wait()
	close(stop)

	if peak.Load() > 16*1024 {
		t.Fatalf("budget exceeded: peak %d > 16384", peak.Load())
	}
}

func TestByteBudgetExhaustionWaitsThenFails(t *testing.T) {
	b := NewByteBudget(4 * 1024)
	if err := b.Acquire(context.Background(), 4*1024); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Acquire(ctx, 1024); err == nil {
		t.Fatal("expected exhaustion error under full budget")
	}
	b.Release(4 * 1024)
	if err := b.Acquire(context.Background(), 4*1024); err != nil {
		t.Fatalf("release did not restore budget: %v", err)
	}
	b.Release(4 * 1024)
	if held, _ := b.Stats(); held != 0 {
		t.Fatalf("held not zero after releases: %d", held)
	}
}

func TestByteBudgetOversizedClamped(t *testing.T) {
	b := NewByteBudget(4 * 1024)
	if err := b.Acquire(context.Background(), 1<<20); err != nil {
		t.Fatalf("oversized should clamp not fail: %v", err)
	}
	if held, _ := b.Stats(); held != 4*1024 {
		t.Fatalf("clamped to capacity, held=%d", held)
	}
	b.Release(1 << 20)
}

func TestByteBudgetMinimumCapacity(t *testing.T) {
	b := NewByteBudget(100)
	if err := b.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	b.Release(1)
}
