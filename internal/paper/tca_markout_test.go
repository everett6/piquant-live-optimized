package paper

import (
	"math"
	"testing"
)

func TestTCAMarkoutTracker(t *testing.T) {
	tracker := NewTCAMarkoutTracker()

	baseTime := int64(1_000_000_000_000)

	// Fill 1: Maker BUY filled at 49,995 when mid is 50,000 (+1.0001 bps spread capture)
	tracker.RecordFill(1, "BTCUSDT", 1, 49995.0, 50000.0, baseTime)

	// Tick at +500ms (no horizons reached)
	tracker.OnTick("BTCUSDT", 50001.0, baseTime+500_000_000)
	snap0 := tracker.Snapshot()
	if snap0.TotalFillsAnalyzed != 1 {
		t.Fatalf("expected 1 fill, got %d", snap0.TotalFillsAnalyzed)
	}
	if snap0.MeanMarkout1sBps != 0 {
		t.Fatalf("expected 0 markout before 1s, got %f", snap0.MeanMarkout1sBps)
	}

	// Tick at +1.2s (+1s horizon reached)
	// Price rose to 50,010: markout = 1 * (50010 - 49995)/49995 * 10,000 = +3.0003 bps
	tracker.OnTick("BTCUSDT", 50010.0, baseTime+1_200_000_000)
	snap1 := tracker.Snapshot()
	if snap1.MeanMarkout1sBps <= 0 {
		t.Fatalf("expected positive 1s markout, got %f", snap1.MeanMarkout1sBps)
	}
	if snap1.ToxicFillRatio != 0 {
		t.Fatalf("expected 0 toxic ratio, got %f", snap1.ToxicFillRatio)
	}

	// Fill 2: Adverse Fill (Toxic)
	// BUY filled at 50,000 when mid is 50,000, then price crashes to 49,980 at +1.1s
	fill2Time := baseTime + 2_000_000_000
	tracker.RecordFill(2, "BTCUSDT", 1, 50000.0, 50000.0, fill2Time)
	// Tick at +1.1s after Fill 2: mid = 49980 -> markout = 1 * (49980 - 50000)/50000 * 10000 = -4.0 bps (< -2.0)
	tracker.OnTick("BTCUSDT", 49980.0, fill2Time+1_100_000_000)

	snap2 := tracker.Snapshot()
	if snap2.ToxicFillRatio <= 0 {
		t.Fatalf("expected toxic fill detected, got ratio %f", snap2.ToxicFillRatio)
	}

	// Tick at +12s after Fill 1 (+5s and +10s horizons reached for Fill 1)
	tracker.OnTick("BTCUSDT", 50005.0, baseTime+12_000_000_000)
	snap3 := tracker.Snapshot()
	if snap3.MeanMarkout5sBps == 0 || snap3.MeanMarkout10sBps == 0 {
		t.Fatalf("expected non-zero 5s and 10s markouts, got 5s=%f, 10s=%f", snap3.MeanMarkout5sBps, snap3.MeanMarkout10sBps)
	}

	// Tick at +65s after Fill 1 (+60s horizon reached for Fill 1)
	tracker.OnTick("BTCUSDT", 50020.0, baseTime+65_000_000_000)
	snap4 := tracker.Snapshot()
	if snap4.MeanMarkout60sBps == 0 {
		t.Fatalf("expected non-zero 60s markout, got %f", snap4.MeanMarkout60sBps)
	}
}

func BenchmarkTCAMarkoutRecordFill(b *testing.B) {
	tracker := NewTCAMarkoutTracker()
	baseTime := int64(1_000_000_000_000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tracker.RecordFill(uint64(i), "BTCUSDT", 1, 50000.0, 50000.5, baseTime+int64(i)*1_000_000)
	}
}

func BenchmarkTCAMarkoutOnTick(b *testing.B) {
	tracker := NewTCAMarkoutTracker()
	baseTime := int64(1_000_000_000_000)
	for i := 0; i < TCAMarkoutRingCapacity; i++ {
		tracker.RecordFill(uint64(i), "BTCUSDT", 1, 50000.0, 50000.5, baseTime+int64(i)*100_000_000)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tracker.OnTick("BTCUSDT", 50005.0, baseTime+Horizon10sNs+int64(i)*1_000_000)
	}
}

func BenchmarkTCAMarkoutSnapshot(b *testing.B) {
	tracker := NewTCAMarkoutTracker()
	baseTime := int64(1_000_000_000_000)
	for i := 0; i < 64; i++ {
		tracker.RecordFill(uint64(i), "BTCUSDT", 1, 50000.0, 50000.5, baseTime+int64(i)*100_000_000)
	}
	tracker.OnTick("BTCUSDT", 50005.0, baseTime+Horizon10sNs)

	b.ResetTimer()
	b.ReportAllocs()
	var snap TCAMarkoutSnapshot
	for i := 0; i < b.N; i++ {
		snap = tracker.Snapshot()
	}
	if math.IsNaN(snap.MeanSpreadCaptureBps) {
		b.Fatal("unexpected NaN")
	}
}
