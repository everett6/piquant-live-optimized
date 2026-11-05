package engine

import (
	"sync"
	"testing"
)

func TestSPSCRingPushPop(t *testing.T) {
	ring := NewSPSCRing(4)
	if ring.Cap() != 4 {
		t.Fatalf("expected capacity 4, got %d", ring.Cap())
	}
	if ring.Len() != 0 {
		t.Fatalf("expected initial len 0, got %d", ring.Len())
	}

	// Pop from empty
	if _, ok := ring.Pop(); ok {
		t.Errorf("expected Pop on empty ring to return false")
	}

	// Push 4 items (fill buffer)
	for i := uint64(1); i <= 4; i++ {
		ev := MarketEvent{Sequence: i, Price: 100.0 * float64(i)}
		if !ring.Push(ev) {
			t.Fatalf("expected Push(%d) to succeed", i)
		}
	}

	if ring.Len() != 4 {
		t.Errorf("expected len 4, got %d", ring.Len())
	}

	// Buffer is now full; 5th push must fail
	overflowEv := MarketEvent{Sequence: 5}
	if ring.Push(overflowEv) {
		t.Errorf("expected Push on full ring to return false")
	}

	// Pop first 2 items
	ev1, ok1 := ring.Pop()
	if !ok1 || ev1.Sequence != 1 {
		t.Errorf("expected sequence 1, got %v", ev1)
	}
	ev2, ok2 := ring.Pop()
	if !ok2 || ev2.Sequence != 2 {
		t.Errorf("expected sequence 2, got %v", ev2)
	}

	if ring.Len() != 2 {
		t.Errorf("expected len 2, got %d", ring.Len())
	}

	// Push 2 more items across circular wrap-around
	if !ring.Push(MarketEvent{Sequence: 5}) {
		t.Errorf("expected push 5 to succeed")
	}
	if !ring.Push(MarketEvent{Sequence: 6}) {
		t.Errorf("expected push 6 to succeed")
	}

	// Drain remaining 4 items
	expectedSeqs := []uint64{3, 4, 5, 6}
	for _, expected := range expectedSeqs {
		ev, ok := ring.Pop()
		if !ok || ev.Sequence != expected {
			t.Errorf("expected sequence %d, got ok=%v, seq=%d", expected, ok, ev.Sequence)
		}
	}

	if ring.Len() != 0 {
		t.Errorf("expected len 0 after drain, got %d", ring.Len())
	}
}

func TestSPSCRingConcurrentProducerConsumer(t *testing.T) {
	ring := NewSPSCRing(1024)
	total := 100_000

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer
	go func() {
		defer wg.Done()
		for i := 1; i <= total; i++ {
			ev := MarketEvent{Sequence: uint64(i), Price: float64(i)}
			for !ring.Push(ev) {
				// busy wait / spin on full
			}
		}
	}()

	// Consumer
	go func() {
		defer wg.Done()
		expectedSeq := uint64(1)
		for expectedSeq <= uint64(total) {
			ev, ok := ring.Pop()
			if !ok {
				continue
			}
			if ev.Sequence != expectedSeq {
				t.Errorf("out of order event: expected %d, got %d", expectedSeq, ev.Sequence)
			}
			expectedSeq++
		}
	}()

	wg.Wait()
}

func BenchmarkSPSCRingPushPop(b *testing.B) {
	ring := NewSPSCRing(1024)
	ev := MarketEvent{
		Timestamp: 1700000000000,
		Sequence:  42,
		Price:     65000.5,
		Size:      1.25,
		Side:      1,
		Kind:      0,
		SymbolID:  1,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ring.Push(ev)
		ring.Pop()
	}
}
