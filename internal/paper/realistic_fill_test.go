package paper

import (
	"context"
	"testing"
	"time"

	"piquant-live/internal/engine"
)

func TestRealisticQueueFillsAndLatency(t *testing.T) {
	tempDir := t.TempDir()
	cfg := DefaultDryRunConfig(tempDir)
	cfg.OrderLatency = 20 * time.Millisecond // 20ms test latency

	ring := engine.NewSPSCRing(1024)
	stream := NewMarketStreamIngestor(DefaultStreamConfig(), ring)

	dryRun, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dryRun.Start(ctx)
	defer dryRun.Stop()

	// 1. Send depth event to establish market and place maker quote at bid ~60000
	midPrice := 60000.0
	ring.Push(engine.MarketEvent{
		Timestamp: time.Now().UnixNano(),
		Sequence:  1,
		Price:     midPrice,
		Size:      2.0,
		Side:      0,
		Kind:      1, // Depth
		SymbolID:  0, // BTCUSDT
	})

	time.Sleep(10 * time.Millisecond)

	// Check that resting maker order is placed
	dryRun.mu.RLock()
	orders := dryRun.activeOrders["BTCUSDT"]
	orderCount := len(orders)
	dryRun.mu.RUnlock()

	if orderCount == 0 {
		t.Fatalf("expected resting maker orders to be placed after depth event")
	}

	// 2. Immediate trade before latency expires (order is still in-flight, ActiveAt not reached)
	ring.Push(engine.MarketEvent{
		Timestamp: time.Now().UnixNano(),
		Sequence:  2,
		Price:     midPrice * 0.9999, // at bid
		Size:      10.0,
		Side:      2, // Sell
		Kind:      0, // Trade
		SymbolID:  0,
	})

	time.Sleep(5 * time.Millisecond)
	st := dryRun.State()
	if st.TotalTrades != 0 {
		t.Fatalf("expected 0 fills before order latency expires, got %d", st.TotalTrades)
	}

	// 3. Wait for latency (20ms) to pass so order becomes active
	time.Sleep(30 * time.Millisecond)

	// 4. Trade through the bid price -> should fill immediately!
	ring.Push(engine.MarketEvent{
		Timestamp: time.Now().UnixNano(),
		Sequence:  3,
		Price:     midPrice * 0.9990, // below bid (through the price)
		Size:      1.0,
		Side:      2, // Sell hitting bids
		Kind:      0, // Trade
		SymbolID:  0,
	})

	time.Sleep(30 * time.Millisecond)

	stAfter := dryRun.State()
	if stAfter.TotalTrades < 1 {
		t.Fatalf("expected maker fill after trade through price, got %d trades", stAfter.TotalTrades)
	}
	fills := dryRun.RecentFills()
	if len(fills) == 0 {
		t.Fatalf("expected fill recorded in recent fills")
	}
	lastFill := fills[len(fills)-1]
	if lastFill.Role != "MAKER" {
		t.Errorf("expected role MAKER, got %s", lastFill.Role)
	}
	if lastFill.DataSource != "live" {
		t.Errorf("expected DataSource live, got %s", lastFill.DataSource)
	}
}

func TestFallbackIsolationNeverDrivesPnL(t *testing.T) {
	tempDir := t.TempDir()
	cfg := DefaultDryRunConfig(tempDir)

	ring := engine.NewSPSCRing(1024)
	streamCfg := DefaultStreamConfig()
	streamCfg.EnableFallback = true
	stream := NewMarketStreamIngestor(streamCfg, ring)

	dryRun, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	// Simulate fallback active
	stream.fallback.Store(true)

	// Send synthetic ticks
	now := time.Now()
	for i := 0; i < 10; i++ {
		ev := engine.MarketEvent{
			Timestamp: now.Add(time.Duration(i) * 100 * time.Millisecond).UnixNano(),
			Sequence:  uint64(i + 1),
			Price:     999999.0, // Crazy synthetic price
			Size:      50.0,
			Side:      1,
			Kind:      0, // Trade
			SymbolID:  0,
		}
		dryRun.processMarketEvent(&ev)
	}

	st := dryRun.State()
	if st.TotalTrades != 0 {
		t.Errorf("expected 0 trades during synthetic fallback, got %d", st.TotalTrades)
	}
	if st.DataSource != "fallback" {
		t.Errorf("expected DataSource fallback, got %s", st.DataSource)
	}
	if !st.QuotingPaused {
		t.Errorf("expected QuotingPaused=true during fallback")
	}
	pos := st.Positions["BTCUSDT"]
	if pos.MarkPrice == 999999.0 {
		t.Errorf("marks should remain frozen during fallback, but mark price updated to %v", pos.MarkPrice)
	}
	if st.Equity != 1000.00 {
		t.Errorf("equity should remain frozen at initial capital, got %v", st.Equity)
	}
}
