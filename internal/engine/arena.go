package engine

import (
	"sync"
)

// EventArena is a fixed-capacity contiguous memory arena and buffer pool designed
// for the Raspberry Pi 5 (Cortex-A76) to guarantee zero runtime GC allocations
// during peak market bursts (5,000+ events/sec).
//
// All MarketEvent instances reside in a contiguous backing slice.
// Alloc() and Free() are O(1) and guaranteed to be zero-allocation (0 B/op, 0 allocs/op).
type EventArena struct {
	mu       sync.Mutex
	storage  []MarketEvent
	freeList []*MarketEvent
	head     int // Next free pointer index
	capacity int
}

// BufferPool is an alias for EventArena representing the reusable memory pool.
type BufferPool = EventArena

// NewEventArena creates a pre-allocated contiguous memory arena of the specified capacity.
func NewEventArena(capacity int) *EventArena {
	if capacity <= 0 {
		capacity = 16384
	}
	storage := make([]MarketEvent, capacity)
	freeList := make([]*MarketEvent, capacity)
	for i := 0; i < capacity; i++ {
		freeList[i] = &storage[i]
	}
	return &EventArena{
		storage:  storage,
		freeList: freeList,
		head:     capacity,
		capacity: capacity,
	}
}

// NewBufferPool creates a pre-allocated buffer pool with default capacity.
func NewBufferPool(capacity int) *BufferPool {
	return NewEventArena(capacity)
}

// Alloc leases an event pointer from the pre-allocated pool with zero heap allocations.
// If the pool is exhausted, returns nil to preserve backpressure without triggering GC allocations.
func (a *EventArena) Alloc() *MarketEvent {
	a.mu.Lock()
	if a.head == 0 {
		a.mu.Unlock()
		return nil
	}
	a.head--
	ptr := a.freeList[a.head]
	a.mu.Unlock()

	// Zero out fields
	*ptr = MarketEvent{}
	return ptr
}

// Free returns an event pointer back to the arena with zero heap allocations.
func (a *EventArena) Free(ev *MarketEvent) {
	if ev == nil {
		return
	}
	a.mu.Lock()
	if a.head < a.capacity {
		*ev = MarketEvent{}
		a.freeList[a.head] = ev
		a.head++
	}
	a.mu.Unlock()
}

// Reset returns all elements back into the arena free pool and zeroes the storage.
func (a *EventArena) Reset() {
	a.mu.Lock()
	var zero MarketEvent
	for i := 0; i < a.capacity; i++ {
		a.storage[i] = zero
		a.freeList[i] = &a.storage[i]
	}
	a.head = a.capacity
	a.mu.Unlock()
}

// Cap returns the total fixed capacity of the arena.
func (a *EventArena) Cap() int {
	return a.capacity
}

// Available returns the number of currently free items in the arena.
func (a *EventArena) Available() int {
	a.mu.Lock()
	n := a.head
	a.mu.Unlock()
	return n
}

// InUse returns the number of items currently leased out of the arena.
func (a *EventArena) InUse() int {
	a.mu.Lock()
	n := a.capacity - a.head
	a.mu.Unlock()
	return n
}
