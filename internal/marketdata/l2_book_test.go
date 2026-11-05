package marketdata

import (
	"testing"
)

func TestL2OrderBookSnapshotAndDelta(t *testing.T) {
	book := NewL2OrderBook("BTCUSDT")
	if !book.NeedsResync() {
		t.Fatalf("expected NeedsResync true on uninitialized book")
	}

	// 1. Initial Snapshot
	initialBids := []PriceLevel{
		{Price: 100.0, Size: 1.5},
		{Price: 99.0, Size: 2.0},
		{Price: 98.0, Size: 3.0},
	}
	initialAsks := []PriceLevel{
		{Price: 101.0, Size: 1.0},
		{Price: 102.0, Size: 2.5},
		{Price: 103.0, Size: 4.0},
	}
	book.SetSnapshot(1000, initialBids, initialAsks)

	if book.NeedsResync() {
		t.Fatalf("expected NeedsResync false after snapshot")
	}
	mid, ok := book.MidPrice()
	if !ok || mid != 100.5 {
		t.Fatalf("expected mid 100.5, got %v (%t)", mid, ok)
	}
	if sp := book.Spread(); sp != 1.0 {
		t.Fatalf("expected spread 1.0, got %f", sp)
	}

	// 2. Continuous delta: update from 1000 to 1005 (pu = 1000)
	delta1 := OrderBookDelta{
		U:  1001,
		u:  1005,
		pu: 1000,
		Bids: []PriceLevel{
			{Price: 100.5, Size: 0.5}, // new best bid inside spread
			{Price: 98.0, Size: 0.0},  // remove 98.0
		},
		Asks: []PriceLevel{
			{Price: 101.0, Size: 1.2}, // update size of best ask
		},
	}

	if !book.ApplyDelta(delta1) {
		t.Fatalf("expected ApplyDelta to succeed")
	}
	if book.LastUpdateID != 1005 {
		t.Fatalf("expected LastUpdateID 1005, got %d", book.LastUpdateID)
	}
	bp, bs := book.BestBid()
	if bp != 100.5 || bs != 0.5 {
		t.Fatalf("expected best bid 100.5 (0.5), got %f (%f)", bp, bs)
	}
	ap, as := book.BestAsk()
	if ap != 101.0 || as != 1.2 {
		t.Fatalf("expected best ask 101.0 (1.2), got %f (%f)", ap, as)
	}

	// 3. Stale / duplicate delta (u <= lastUpdateID): should be ignored cleanly without gap
	deltaStale := OrderBookDelta{
		U:  1002,
		u:  1004,
		pu: 1001,
	}
	if !book.ApplyDelta(deltaStale) || book.NeedsResync() {
		t.Fatalf("expected stale delta to be safely ignored without gap")
	}

	// 4. Sequence gap detection: next delta has pu = 1010 != 1005
	deltaGap := OrderBookDelta{
		U:  1011,
		u:  1015,
		pu: 1010, // gap!
	}
	if book.ApplyDelta(deltaGap) {
		t.Fatalf("expected ApplyDelta to return false on sequence gap")
	}
	if !book.NeedsResync() {
		t.Fatalf("expected NeedsResync true after sequence gap")
	}
	if book.GapCount != 1 {
		t.Fatalf("expected GapCount 1, got %d", book.GapCount)
	}
}

func BenchmarkL2OrderBookApplyDelta(b *testing.B) {
	book := NewL2OrderBook("BTCUSDT")
	initialBids := make([]PriceLevel, 20)
	initialAsks := make([]PriceLevel, 20)
	for i := 0; i < 20; i++ {
		initialBids[i] = PriceLevel{Price: 100.0 - float64(i)*0.1, Size: float64(i + 1)}
		initialAsks[i] = PriceLevel{Price: 100.1 + float64(i)*0.1, Size: float64(i + 1)}
	}
	book.SetSnapshot(100, initialBids, initialAsks)

	// Pre-create fixed arrays for delta
	bidUps := [2]PriceLevel{
		{Price: 99.95, Size: 1.5},
		{Price: 99.85, Size: 2.0},
	}
	askUps := [2]PriceLevel{
		{Price: 100.15, Size: 1.8},
		{Price: 100.25, Size: 2.2},
	}

	delta := OrderBookDelta{
		U:    101,
		u:    101,
		pu:   100,
		Bids: bidUps[:],
		Asks: askUps[:],
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		delta.pu = book.LastUpdateID
		delta.U = book.LastUpdateID + 1
		delta.u = delta.U
		book.ApplyDelta(delta)
	}
}
