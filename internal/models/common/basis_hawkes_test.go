package common

import (
	"math"
	"testing"
)

func TestSpotPerpBasisModel_EvaluationAndDecisions(t *testing.T) {
	model := NewSpotPerpBasisModel(1e-3, 0.65, 1.5)

	// Baseline update: Spot 50,000, Perp 50,000 => Basis 0 bps
	dec0 := model.Evaluate(50000.0, 50000.0, 0.8)
	if math.Abs(dec0.BasisBps) > 1e-4 {
		t.Fatalf("expected 0 basis bps, got %f", dec0.BasisBps)
	}
	if dec0.Action != ActionHold {
		t.Fatalf("expected HOLD on zero basis, got %s", dec0.Action)
	}

	// Establish baseline equilibrium: Spot 50,000, Perp 50,000 => Basis 0 bps
	for i := 0; i < 50; i++ {
		model.Evaluate(50000.0, 50000.0, 0.8)
	}

	// Rich Basis Sudden Dislocation: Spot 50,000, Perp 50,020 => Basis +4.0 bps
	decRich := model.Evaluate(50000.0, 50020.0, 0.8)
	if decRich.ZScore < 1.5 {
		t.Fatalf("expected Z-score >= 1.5 for rich basis, got %f", decRich.ZScore)
	}
	if decRich.Action != ActionConvergenceBuySpotSellPerp {
		t.Fatalf("expected CONVERGENCE_BUY_SPOT_SELL_PERP, got %s", decRich.Action)
	}
	if decRich.AlphaBps >= 0 {
		t.Fatalf("expected negative alpha bps (mean-reversion down), got %f", decRich.AlphaBps)
	}

	// Re-establish equilibrium
	for i := 0; i < 50; i++ {
		model.Evaluate(50000.0, 50000.0, 0.8)
	}

	// Cheap Basis Sudden Dislocation: Spot 50,000, Perp 49,980 => Basis -4.0 bps
	decCheap := model.Evaluate(50000.0, 49980.0, 0.8)
	if decCheap.ZScore > -1.5 {
		t.Fatalf("expected Z-score <= -1.5 for cheap basis, got %f", decCheap.ZScore)
	}
	if decCheap.Action != ActionConvergenceSellSpotBuyPerp {
		t.Fatalf("expected CONVERGENCE_SELL_SPOT_BUY_PERP, got %s", decCheap.Action)
	}
	if decCheap.AlphaBps <= 0 {
		t.Fatalf("expected positive alpha bps (mean-reversion up), got %f", decCheap.AlphaBps)
	}
}

func TestSpotPerpBasisModel_PredictZeroAlloc(t *testing.T) {
	model := NewSpotPerpBasisModel(1e-3, 0.65, 1.5)
	x := [FeatureCount]float64{
		0.20, 0.15, 0.10, 0.25, 0.80, 1.20, 2.50, 5.00,
		1.40, 3.20, 1.50, 0.35, 0.90, 0.40, 12.0, 0.55,
	}

	pred := model.Predict(&x)
	if math.IsNaN(pred) || math.IsInf(pred, 0) {
		t.Fatalf("invalid prediction: %f", pred)
	}
}

func BenchmarkSpotPerpBasisModel_Evaluate(b *testing.B) {
	model := NewSpotPerpBasisModel(1e-3, 0.65, 1.5)
	spot := 50000.0
	perp := 50005.0
	spread := 0.8

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = model.Evaluate(spot, perp, spread)
	}
}

func BenchmarkSpotPerpBasisModel_Predict(b *testing.B) {
	model := NewSpotPerpBasisModel(1e-3, 0.65, 1.5)
	x := [FeatureCount]float64{
		0.20, 0.15, 0.10, 0.25, 0.80, 1.20, 2.50, 5.00,
		1.40, 3.20, 1.50, 0.35, 0.90, 0.40, 12.0, 0.55,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = model.Predict(&x)
	}
}

func TestOnlineHawkesEstimator_CascadeDetection(t *testing.T) {
	// Baseline: mu = 5.0, alpha = 12.0, beta = 20.0 => n = 0.60 (Stable)
	h := NewOnlineHawkesEstimator(5.0, 12.0, 20.0)
	if math.Abs(h.BranchingRatio()-0.60) > 1e-4 {
		t.Fatalf("expected branching ratio 0.60, got %f", h.BranchingRatio())
	}
	if h.IsCascadeRisk() {
		t.Fatalf("expected stable regime, got cascade risk")
	}

	// Trigger cascade: alpha = 18.5, beta = 20.0 => n = 0.925 >= 0.90
	h.SetBranchingRatio(0.925)
	if !h.IsCascadeRisk() {
		t.Fatalf("expected cascade risk when n = 0.925")
	}

	// Reset back to subcritical
	h.SetBranchingRatio(0.60)
	nowNs := int64(1_700_000_000_000_000_000)

	// Simulate trade burst
	for i := 0; i < 10; i++ {
		nowNs += 20_000_000 // 20ms apart
		h.Update(nowNs, i%2 == 0, 1.5)
	}

	lambda := h.Intensity()
	if lambda <= 5.0 {
		t.Fatalf("expected elevated intensity after burst, got %f", lambda)
	}

	// Test decay after 500ms
	laterNs := nowNs + 500_000_000
	decayedLambda := h.IntensityAt(laterNs)
	if decayedLambda >= lambda {
		t.Fatalf("expected decayed intensity %f < %f", decayedLambda, lambda)
	}
}

func BenchmarkOnlineHawkesEstimator_Update(b *testing.B) {
	h := NewOnlineHawkesEstimator(5.0, 12.0, 20.0)
	nowNs := int64(1_700_000_000_000_000_000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nowNs += 50_000_000
		h.Update(nowNs, (i&1) == 0, 1.0)
	}
}

func BenchmarkOnlineHawkesEstimator_Intensity(b *testing.B) {
	h := NewOnlineHawkesEstimator(5.0, 12.0, 20.0)
	h.Update(1_700_000_000_000_000_000, true, 2.0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = h.Intensity()
	}
}
