package engine

import (
	"math/bits"
	"sync/atomic"
)

// MarketEvent represents a fixed-size market data event (tick/depth/trade)
// suitable for cross-core lock-free transfer without heap allocation.
type MarketEvent struct {
	Timestamp int64   // Venue or local timestamp (nanoseconds)
	Sequence  uint64  // Venue sequence number
	Price     float64 // Trade price or best mid price
	Size      float64 // Trade size or depth volume
	Side      uint8   // 0: Unknown, 1: Buy, 2: Sell
	Kind      uint8   // 0: Trade, 1: Depth, 2: Snapshot, 3: Heartbeat
	SymbolID  uint16  // Normalized instrument ID
	Flags     uint32  // Operational flags
}

// CacheLineSize is 64 bytes on AMD64 and ARM64 Cortex-A76.
const CacheLineSize = 64

// SPSCRing is a bounded, high-throughput, lock-free Single-Producer Single-Consumer
// ring buffer for passing MarketEvents across isolated CPU cores (e.g. isolcpus 2,3)
// with zero memory allocations on the hot path.
//
// Uses atomic.Uint64 with 64-byte ([56]byte) cache-line padding between producer
// and consumer fields to completely eliminate CPU false sharing.
type SPSCRing struct {
	_ [CacheLineSize]byte

	// Producer fields (written by producer only)
	head atomic.Uint64
	_    [CacheLineSize - 8]byte // 56 bytes padding to align to next cacheline

	// Consumer fields (written by consumer only)
	tail atomic.Uint64
	_    [CacheLineSize - 8]byte // 56 bytes padding to align to next cacheline

	mask uint64
	buf  []MarketEvent
}

// NewSPSCRing allocates an SPSCRing with capacity rounded up to the next power of two.
func NewSPSCRing(capacity int) *SPSCRing {
	if capacity <= 0 {
		capacity = 1024
	}
	if capacity&(capacity-1) != 0 {
		capacity = 1 << bits.Len(uint(capacity))
	}
	return &SPSCRing{
		mask: uint64(capacity - 1),
		buf:  make([]MarketEvent, capacity),
	}
}

// Cap returns the total capacity of the ring buffer.
func (r *SPSCRing) Cap() int {
	return len(r.buf)
}

// Len returns the instantaneous number of unconsumed elements in the ring buffer.
func (r *SPSCRing) Len() int {
	h := r.head.Load()
	t := r.tail.Load()
	if h >= t {
		return int(h - t)
	}
	return 0
}

// Push adds an event to the ring buffer.
// Returns false if the ring buffer is full (producer must retry or handle backpressure).
// Zero allocations (0 B/op, 0 allocs/op).
func (r *SPSCRing) Push(event MarketEvent) bool {
	h := r.head.Load()
	t := r.tail.Load()

	if h-t >= uint64(len(r.buf)) {
		return false // Ring full
	}

	r.buf[h&r.mask] = event
	r.head.Store(h + 1)
	return true
}

// Pop extracts the oldest event from the ring buffer.
// Returns (zeroValue, false) if the ring buffer is empty.
// Zero allocations (0 B/op, 0 allocs/op).
func (r *SPSCRing) Pop() (MarketEvent, bool) {
	t := r.tail.Load()
	h := r.head.Load()

	if t == h {
		return MarketEvent{}, false // Ring empty
	}

	ev := r.buf[t&r.mask]
	r.tail.Store(t + 1)
	return ev, true
}

// Reset clears the head and tail cursors. Must be called only when quiescent.
func (r *SPSCRing) Reset() {
	r.head.Store(0)
	r.tail.Store(0)
	var zero MarketEvent
	for i := range r.buf {
		r.buf[i] = zero
	}
}
