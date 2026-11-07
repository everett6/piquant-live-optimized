package backtest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTickSimulatorLifecycle(t *testing.T) {
	cfg := DefaultTickSimConfig()
	cfg.InitialCapital = 1000.00
	cfg.Symbol = "BTCUSDT"

	sim := NewTickSimulator(cfg)

	// Stream 500 synthetic ticks
	basePrice := 50000.00
	baseTime := int64(1700000000000000000)

	for i := 0; i < 500; i++ {
		tNs := baseTime + int64(i*8_000_000) // 8ms ticks
		price := basePrice + float64(i%20)*0.5 - float64(i%15)*0.3
		qty := 0.05 + float64(i%5)*0.01
		isBuyerMaker := (i%2 == 0)

		tick := AggTradeTick{
			TradeID:      uint64(1000 + i),
			Price:        price,
			Quantity:     qty,
			FirstTradeID: uint64(1000 + i),
			LastTradeID:  uint64(1000 + i),
			TimestampNs:  tNs,
			IsBuyerMaker: isBuyerMaker,
			IsBestMatch:  true,
		}

		sim.ProcessTick(&tick)
	}

	snap := sim.MonthlySnapshot("2024-01", 1000.00)
	if snap.TotalTicks != 500 {
		t.Errorf("expected 500 total ticks, got %d", snap.TotalTicks)
	}
	if snap.EndEquity <= 0 {
		t.Errorf("expected positive equity, got %f", snap.EndEquity)
	}

	res := sim.FinalizeResult([]MonthlyTickMetric{snap})
	if res.Symbol != "BTCUSDT" {
		t.Errorf("expected BTCUSDT symbol, got %s", res.Symbol)
	}
	if res.InitialCapital != 1000.00 {
		t.Errorf("expected 1000 initial capital, got %f", res.InitialCapital)
	}

	tmpDir := t.TempDir()
	mdPath, jsonPath, err := GenerateTickReports(tmpDir, res)
	if err != nil {
		t.Fatalf("GenerateTickReports failed: %v", err)
	}

	if _, err := os.Stat(mdPath); err != nil {
		t.Errorf("missing generated markdown report: %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Errorf("missing generated json report: %v", err)
	}
}

func BenchmarkTickSimulator_ProcessTick(b *testing.B) {
	cfg := DefaultTickSimConfig()
	sim := NewTickSimulator(cfg)

	tick := AggTradeTick{
		TradeID:      10001,
		Price:        50000.00,
		Quantity:     0.05,
		FirstTradeID: 10001,
		LastTradeID:  10001,
		TimestampNs:  1700000000000000000,
		IsBuyerMaker: true,
		IsBestMatch:  true,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		tick.TradeID = uint64(i + 1)
		tick.TimestampNs += 8_000_000 // +8ms
		sim.ProcessTick(&tick)
	}
}

func TestPQBinStreamRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	pqPath := filepath.Join(tmpDir, "test-stream.pqbin")

	ticks := make([]AggTradeTick, 100)
	for i := 0; i < 100; i++ {
		ticks[i] = AggTradeTick{
			TradeID:      uint64(20000 + i),
			Price:        65000.0 + float64(i)*0.1,
			Quantity:     0.01 + float64(i)*0.001,
			FirstTradeID: uint64(20000 + i),
			LastTradeID:  uint64(20000 + i),
			TimestampNs:  1704067200000000000 + int64(i*5000000),
			IsBuyerMaker: (i%3 == 0),
			IsBestMatch:  true,
		}
	}

	if err := SavePQBin(pqPath, "BTCUSDT", 2024, 1, ticks); err != nil {
		t.Fatalf("SavePQBin error: %v", err)
	}

	count := 0
	_, err := LoadPQBinFile(pqPath, func(tick *AggTradeTick) error {
		if tick.TradeID != ticks[count].TradeID {
			t.Errorf("tick %d ID mismatch: %d vs %d", count, tick.TradeID, ticks[count].TradeID)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("LoadPQBinFile error: %v", err)
	}
	if count != 100 {
		t.Errorf("expected 100 ticks streamed, got %d", count)
	}
}
