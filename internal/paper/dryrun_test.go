package paper

import (
	"context"
	"os"
	"testing"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/models/common"
)

func TestDryRunEngineLifecycleAndCheckpoint(t *testing.T) {
	tempDir := t.TempDir()
	cfg := DefaultDryRunConfig(tempDir)
	cfg.CheckpointFreq = 10 * time.Millisecond
	cfg.TargetDuration = 14 * 24 * time.Hour

	ring := engine.NewSPSCRing(1024)
	stream := NewMarketStreamIngestor(DefaultStreamConfig(), ring)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dryRun, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		t.Fatalf("failed to create dry run engine: %v", err)
	}

	dryRun.Start(ctx)

	// Verify initial state
	st := dryRun.State()
	if st.InitialCapital != 100000.00 {
		t.Errorf("expected initial capital 100,000, got %v", st.InitialCapital)
	}
	if st.Equity != 100000.00 {
		t.Errorf("expected equity 100,000, got %v", st.Equity)
	}
	if st.DaysElapsed < 1 || st.DaysElapsed > 14 {
		t.Errorf("expected days elapsed in [1, 14], got %v", st.DaysElapsed)
	}
	if st.HoursRemaining < 330 || st.HoursRemaining > 337 {
		t.Errorf("expected hours remaining ~336h, got %v", st.HoursRemaining)
	}

	// Push 20 ticks into the ring (depth and trades)
	nowNs := time.Now().UnixNano()
	for i := 0; i < 20; i++ {
		ring.Push(engine.MarketEvent{
			Timestamp: nowNs + int64(i*1000000),
			Sequence:  uint64(1000 + i),
			Price:     65000.0 + float64(i)*0.5,
			Size:      1.5,
			Side:      uint8(1 + (i % 2)),
			Kind:      uint8(i % 2),
			SymbolID:  0, // BTCUSDT
		})
	}

	// Give execution loop time to process
	time.Sleep(150 * time.Millisecond)

	stAfter := dryRun.State()
	t.Logf("Processed ticks: TotalTrades=%d RealizedPnL=%.2f Equity=%.2f Return=%.4f%%",
		stAfter.TotalTrades, stAfter.RealizedPnL, stAfter.Equity, stAfter.TotalReturnPct)

	// Stop engine and ensure checkpoint file exists
	dryRun.Stop()

	if _, err := os.Stat(cfg.StateFile); os.IsNotExist(err) {
		t.Fatalf("checkpoint state file %s does not exist", cfg.StateFile)
	}

	// Test seamless recovery / resume after restart/reboot
	dryRun2, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		t.Fatalf("failed to recover dry run engine: %v", err)
	}

	stRecovered := dryRun2.State()
	if !stRecovered.StartTime.Equal(st.StartTime) {
		t.Errorf("expected recovered start time %v to match %v", stRecovered.StartTime, st.StartTime)
	}
	if stRecovered.TotalTrades != stAfter.TotalTrades {
		t.Errorf("expected recovered total trades %d to match %d", stRecovered.TotalTrades, stAfter.TotalTrades)
	}
	if stRecovered.Equity != stAfter.Equity {
		t.Errorf("expected recovered equity %.2f to match %.2f", stRecovered.Equity, stAfter.Equity)
	}
}

func TestCircuitBreakerEmergencyHalt(t *testing.T) {
	tempDir := t.TempDir()
	cfg := DefaultDryRunConfig(tempDir)
	cfg.MaxDrawdownPct = 2.0 // 2% limit

	ring := engine.NewSPSCRing(1024)
	stream := NewMarketStreamIngestor(DefaultStreamConfig(), ring)

	dryRun, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		t.Fatalf("failed to create dry run engine: %v", err)
	}

	// Simulate long position with massive drop to trigger >2% max drawdown
	dryRun.mu.Lock()
	dryRun.state.Cash = 0
	dryRun.state.Equity = 100000.00
	dryRun.state.PeakEquity = 100000.00
	dryRun.state.Positions["BTCUSDT"] = &Position{
		Symbol:        "BTCUSDT",
		Size:          2.0,
		AvgEntryPrice: 50000.0,
	}
	dryRun.mu.Unlock()

	// Price drops by 1500 (from 50000 to 48500 -> 2.0 * -1500 = -3000 USDT loss = 3% drawdown)
	ev := engine.MarketEvent{
		Timestamp: time.Now().UnixNano(),
		Sequence:  101,
		Price:     48500.0,
		Size:      1.0,
		Side:      2,
		Kind:      0,
		SymbolID:  0,
	}
	dryRun.processMarketEvent(&ev)

	st := dryRun.State()
	if !st.CircuitHalted {
		t.Errorf("expected circuit breaker to be halted due to >2%% drawdown, but was not halted")
	}
	if st.MaxDrawdownPct < 2.0 {
		t.Errorf("expected MaxDrawdownPct >= 2.0%%, got %.2f%%", st.MaxDrawdownPct)
	}
	t.Logf("Circuit Breaker successfully triggered: Reason=%s (MaxDD=%.2f%%)", st.CircuitHaltReason, st.MaxDrawdownPct)
}

func TestContDeLarrardAndKylesLambdaExecution(t *testing.T) {
	tempDir := t.TempDir()
	cfg := DefaultDryRunConfig(tempDir)
	cfg.KylesLambda = 0.50

	ring := engine.NewSPSCRing(1024)
	stream := NewMarketStreamIngestor(DefaultStreamConfig(), ring)

	dryRun, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		t.Fatalf("failed to create dry run: %v", err)
	}

	// Test Maker Fill with GLFT spread capture
	qd := common.QuoteDecision{
		ReservationBps: 0.50,
		HalfSpreadBps:  1.20,
		BidOffsetBps:   0.70,
		AskOffsetBps:   1.70,
	}
	dryRun.simulateMakerFill("BTCUSDT", 60000.0, qd, 1.20)

	st := dryRun.State()
	if st.TotalTrades != 1 {
		t.Fatalf("expected 1 maker trade, got %d", st.TotalTrades)
	}
	pos := st.Positions["BTCUSDT"]
	if pos.Size != 0.10 {
		t.Errorf("expected pos size 0.10 BTC, got %v", pos.Size)
	}
	if pos.MakerTrades != 1 {
		t.Errorf("expected 1 maker trade in pos, got %d", pos.MakerTrades)
	}

	// Test Taker Fill with Kyle's Lambda slippage
	dryRun.simulateTakerFill("BTCUSDT", 60000.0, 3.50)
	st2 := dryRun.State()
	if st2.TotalTrades != 2 {
		t.Fatalf("expected 2 total trades, got %d", st2.TotalTrades)
	}
	pos2 := st2.Positions["BTCUSDT"]
	if pos2.TakerTrades != 1 {
		t.Errorf("expected 1 taker trade in pos, got %d", pos2.TakerTrades)
	}

	fills := dryRun.RecentFills()
	if len(fills) != 2 {
		t.Fatalf("expected 2 recent fills, got %d", len(fills))
	}
	if fills[1].Role != "TAKER" || fills[1].SlippageBps <= 0 {
		t.Errorf("expected taker fill with positive Kyle's lambda slippage, got %+v", fills[1])
	}
}

func TestMarketStreamIngestorFallback(t *testing.T) {
	ring := engine.NewSPSCRing(1024)
	cfg := StreamConfig{
		Endpoint:       "wss://stream.binance.com:9443/invalid-path",
		Symbols:        []string{"BTCUSDT", "ETHUSDT"},
		EnableFallback: true,
	}

	stream := NewMarketStreamIngestor(cfg, ring)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream.Start(ctx)

	// Wait for fallback watchdog to generate synthetic ticks into ring
	time.Sleep(350 * time.Millisecond)

	stream.Stop()

	total, depths, trades, _, _, fallbacks, lastNs := stream.Stats()
	if fallbacks == 0 || total == 0 {
		t.Errorf("expected fallback generator to generate events, got fallbacks=%d total=%d", fallbacks, total)
	}
	if lastNs == 0 {
		t.Errorf("expected lastNs > 0")
	}
	t.Logf("Fallback generator verified: Total=%d Depths=%d Trades=%d Fallbacks=%d", total, depths, trades, fallbacks)

	// Verify events were delivered to SPSC ring
	if ring.Len() == 0 {
		t.Errorf("expected ring to have received events")
	}
	popped := 0
	for {
		ev, ok := ring.Pop()
		if !ok {
			break
		}
		popped++
		if ev.Price <= 0 {
			t.Errorf("unexpected invalid price in ring: %v", ev.Price)
		}
	}
	if popped == 0 {
		t.Errorf("expected to pop events from ring")
	}
}

func BenchmarkProcessMarketEvent(b *testing.B) {
	tempDir := b.TempDir()
	cfg := DefaultDryRunConfig(tempDir)
	ring := engine.NewSPSCRing(1024)
	stream := NewMarketStreamIngestor(DefaultStreamConfig(), ring)

	dryRun, err := NewDryRunEngine(cfg, ring, stream)
	if err != nil {
		b.Fatalf("failed to create dry run engine: %v", err)
	}

	ev := engine.MarketEvent{
		Timestamp: time.Now().UnixNano(),
		Sequence:  1000,
		Price:     50000.0,
		Size:      1.5,
		Side:      1,
		Kind:      1,
		SymbolID:  0,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		ev.Sequence = uint64(i)
		dryRun.processMarketEvent(&ev)
	}
}
