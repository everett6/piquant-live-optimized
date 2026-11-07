package engine

import (
	"sync"
	"testing"
)

func TestEventArenaLifecycle(t *testing.T) {
	arena := NewEventArena(16)
	if arena.Cap() != 16 {
		t.Fatalf("expected capacity 16, got %d", arena.Cap())
	}
	if arena.Available() != 16 {
		t.Fatalf("expected available 16, got %d", arena.Available())
	}
	if arena.InUse() != 0 {
		t.Fatalf("expected in-use 0, got %d", arena.InUse())
	}

	// Allocate all 16
	events := make([]*MarketEvent, 16)
	for i := 0; i < 16; i++ {
		ev := arena.Alloc()
		if ev == nil {
			t.Fatalf("expected non-nil event at index %d", i)
		}
		ev.Sequence = uint64(i + 1)
		ev.Price = float64(100 + i)
		events[i] = ev
	}

	if arena.Available() != 0 {
		t.Fatalf("expected available 0, got %d", arena.Available())
	}
	if arena.InUse() != 16 {
		t.Fatalf("expected in-use 16, got %d", arena.InUse())
	}

	// 17th allocation must return nil (pool exhausted)
	if evExtra := arena.Alloc(); evExtra != nil {
		t.Fatalf("expected nil allocation on exhausted arena, got %v", evExtra)
	}

	// Free first 8
	for i := 0; i < 8; i++ {
		arena.Free(events[i])
	}
	if arena.Available() != 8 {
		t.Fatalf("expected available 8, got %d", arena.Available())
	}
	if arena.InUse() != 8 {
		t.Fatalf("expected in-use 8, got %d", arena.InUse())
	}

	// Re-allocating must return zeroed event
	reAlloc := arena.Alloc()
	if reAlloc == nil {
		t.Fatalf("expected valid reallocation")
	}
	if reAlloc.Sequence != 0 || reAlloc.Price != 0 {
		t.Fatalf("expected reallocated event to be zeroed, got seq=%d, price=%f", reAlloc.Sequence, reAlloc.Price)
	}

	// Reset
	arena.Reset()
	if arena.Available() != 16 || arena.InUse() != 0 {
		t.Fatalf("expected full reset to cap 16, got avail=%d inuse=%d", arena.Available(), arena.InUse())
	}
}

func TestEventArenaConcurrent(t *testing.T) {
	const capSize = 1024
	arena := NewEventArena(capSize)
	var wg sync.WaitGroup
	workers := 8
	iterations := 1000

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ev := arena.Alloc()
				if ev != nil {
					ev.Sequence = uint64(workerID*iterations + i)
					ev.Price = float64(i)
					arena.Free(ev)
				}
			}
		}(w)
	}
	wg.Wait()

	if arena.InUse() != 0 {
		t.Fatalf("expected 0 in-use after concurrent workers, got %d", arena.InUse())
	}
}

func BenchmarkEventArenaAllocFree(b *testing.B) {
	arena := NewEventArena(1024)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev := arena.Alloc()
		if ev != nil {
			ev.Sequence = uint64(i)
			arena.Free(ev)
		}
	}
}
