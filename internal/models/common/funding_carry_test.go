package common

import (
	"math"
	"testing"
)

func TestFundingCarryTrackerFormulas(t *testing.T) {
	tr := NewFundingCarryTracker(0.10, 0.0001) // 0.01% base interest

	// 1. Perp trading at spot: premium = 0
	// diff = 0.0001 - 0 = 0.0001, within clamp band [-0.0005, 0.0005]
	// instant funding = 0 + 0.0001 = 0.0001 (1 bps)
	f1 := tr.UpdatePrices(50000.0, 50000.0, 1000)
	if math.Abs(f1-0.0001) > 1e-7 {
		t.Fatalf("expected f1 = 0.0001, got %f", f1)
	}
	if math.Abs(tr.PredictedFundingBps()-1.0) > 1e-3 {
		t.Fatalf("expected 1.0 bps, got %f", tr.PredictedFundingBps())
	}
	// APR = 0.0001 * 1095 * 100 = 10.95%
	expectedAPR := 0.0001 * 1095.0 * 100.0
	if math.Abs(tr.AnnualizedAPR()-expectedAPR) > 1e-3 {
		t.Fatalf("expected APR = %f, got %f", expectedAPR, tr.AnnualizedAPR())
	}

	// 2. Perp trading at premium: perp = 50050, spot = 50000 => premium = +0.001 (10 bps)
	// diff = 0.0001 - 0.001 = -0.0009 => clamped to -0.0005
	// instant funding = 0.001 - 0.0005 = 0.0005 (5 bps)
	f2 := tr.UpdatePrices(50050.0, 50000.0, 2000)
	if math.Abs(f2-0.0005) > 1e-7 {
		t.Fatalf("expected clamped f2 = 0.0005, got %f", f2)
	}

	// 3. Test FundingSkewBps
	// If funding rate is positive (+5 bps):
	// Long inventory (e.g. +1.0) pays funding. As settlement approaches (e.g. 60s remaining),
	// skew should be negative (encouraging selling/shorting to offload paying inventory).
	skewPayingLong := tr.FundingSkewBps(1.0, 60.0)
	if skewPayingLong >= 0 {
		t.Fatalf("expected negative skew for paying long near settlement, got %f", skewPayingLong)
	}

	// If holding short inventory (-1.0) with positive funding (+5 bps), short collects rebate!
	// Compare skew: short inventory should have higher (less penalized) reservation skew than paying long.
	skewReceivingShort := tr.FundingSkewBps(-1.0, 60.0)
	if skewReceivingShort <= skewPayingLong {
		t.Fatalf("expected skewReceivingShort (%f) > skewPayingLong (%f)", skewReceivingShort, skewPayingLong)
	}

	// Time urgency: skew with 10s left should be stronger in magnitude than skew with 28,000s left
	skewUrgent := math.Abs(tr.FundingSkewBps(1.0, 10.0))
	skewCalm := math.Abs(tr.FundingSkewBps(1.0, 28000.0))
	if skewUrgent <= skewCalm {
		t.Fatalf("expected urgent skew (%f) > calm skew (%f)", skewUrgent, skewCalm)
	}
}

func TestFundingCarryModelPredict(t *testing.T) {
	m := NewFundingCarryModel(1e-3)
	m.Tracker.UpdatePrices(50020.0, 50000.0, 1000)

	var x [FeatureCount]float64
	x[0] = 0.2
	x[4] = 0.8
	x[5] = 1.0
	x[11] = 0.2

	pred := m.Predict(&x)
	if math.IsNaN(pred) || math.IsInf(pred, 0) {
		t.Fatalf("invalid prediction: %f", pred)
	}
}

func BenchmarkFundingCarryUpdatePrices(b *testing.B) {
	tr := NewFundingCarryTracker(0.05, 0.0001)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tr.UpdatePrices(50010.0, 50000.0, int64(i)*1_000_000)
	}
}

func BenchmarkFundingCarrySkewBps(b *testing.B) {
	tr := NewFundingCarryTracker(0.05, 0.0001)
	tr.UpdatePrices(50020.0, 50000.0, 1000)

	b.ResetTimer()
	b.ReportAllocs()
	var dummy float64
	for i := 0; i < b.N; i++ {
		dummy += tr.FundingSkewBps(0.5, 3600.0)
	}
	if math.IsNaN(dummy) {
		b.Fatal("unexpected NaN")
	}
}

func BenchmarkFundingCarryModelPredict(b *testing.B) {
	m := NewFundingCarryModel(1e-3)
	m.Tracker.UpdatePrices(50015.0, 50000.0, 1000)
	var x [FeatureCount]float64
	x[0] = 0.3
	x[4] = 1.0
	x[5] = 1.2
	x[11] = 0.1

	b.ResetTimer()
	b.ReportAllocs()
	var dummy float64
	for i := 0; i < b.N; i++ {
		dummy += m.Predict(&x)
	}
	if math.IsNaN(dummy) {
		b.Fatal("unexpected NaN")
	}
}
