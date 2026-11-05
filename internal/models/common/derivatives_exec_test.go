package common

import (
	"math"
	"testing"
)

func TestPerpBasisTrackerAndLiquidationDetector(t *testing.T) {
	tracker := NewPerpBasisTracker(0.1, 0.25)

	// Feed baseline neutral observations
	for i := 0; i < 30; i++ {
		var x [FeatureCount]float64
		x[3] = 0.02 * math.Sin(float64(i)*0.2)
		x[4] = 0.8
		x[6] = 0.2 * math.Cos(float64(i)*0.2)
		tracker.Update(&x)
	}

	// Feed a stretched positive perpetual basis dislocation
	var xBull [FeatureCount]float64
	xBull[0] = 0.65
	xBull[2] = -0.10
	xBull[3] = 0.45
	xBull[4] = 1.2
	xBull[6] = 4.5
	xBull[7] = 8.0
	xBull[9] = 3.8
	xBull[12] = 2.5
	xBull[13] = 2.2
	basisBps, zScore, carryBps := tracker.Update(&xBull)
	if basisBps <= 0 {
		t.Errorf("expected positive basisBps on bullish microprice/OFI skew, got %v", basisBps)
	}
	if zScore <= 0.5 {
		t.Errorf("expected elevated positive basis z-score, got %v", zScore)
	}
	if carryBps >= 0 {
		t.Errorf("expected negative mean-reverting funding settlement pull on stretched positive basis, got %v", carryBps)
	}

	// Test LiquidationCascadeDetector on a severe sell liquidation cascade reaching exhaustion
	det := NewLiquidationCascadeDetector()
	var xCascade [FeatureCount]float64
	xCascade[0] = 0.45  // L1 bid absorption appearing at touch
	xCascade[3] = 0.25  // microprice starting to lift
	xCascade[4] = 4.5   // wide spread
	xCascade[7] = -12.0 // severe 30s book depletion
	xCascade[8] = -5.5  // heavy negative MLOFI
	xCascade[10] = 8.5  // volatility spike
	xCascade[11] = -9.5 // sharp downward velocity
	xCascade[13] = -6.0 // aggressive sell trade imbalance
	xCascade[14] = 45.0 // high trade arrival rate

	score, alphaBps, inCascade := det.Evaluate(&xCascade)
	if !inCascade || score < 0.70 {
		t.Errorf("expected active liquidation cascade (score >= 0.70), got score=%v inCascade=%v", score, inCascade)
	}
	if alphaBps <= 0 {
		t.Errorf("expected positive V-shaped exhaustion reversion alpha after sell cascade climax, got %v", alphaBps)
	}
}

func TestPerpLiquidationReversionModelFitPredictClone(t *testing.T) {
	samples := make([]Sample, 160)
	for i := 0; i < 160; i++ {
		var x [FeatureCount]float64
		x0 := math.Sin(float64(i) * 0.15)
		x[0] = x0
		x[3] = x0 * 0.3
		x[4] = 1.0
		x[7] = -x0 * 3.0
		x[8] = x0 * 1.5
		x[10] = 2.0
		x[11] = x0 * 2.5
		x[13] = x0 * 1.8
		x[14] = 10.0

		y := 1.8*x0 + 0.4*x[8]
		// Inject exhaustion reversion episodes
		if i%20 == 0 {
			x[11] = -8.5
			x[7] = -10.0
			x[8] = -4.5
			x[13] = -5.0
			x[0] = 0.5
			x[3] = 0.3
			y = 4.5 // V-shaped bounce
		}
		samples[i] = Sample{X: x, Y: y}
	}

	mod := NewPerpLiquidationReversionModel(1e-3)
	if mod.Name() != "perp_liquidation_reversion" {
		t.Fatalf("unexpected model name %q", mod.Name())
	}
	idx := []int{0, 3, 4, 7, 8, 10, 11, 13, 14}
	if err := mod.Fit(samples, idx); err != nil {
		t.Fatalf("Fit failed: %v", err)
	}

	pred := mod.Predict(&samples[0].X)
	if !Finite(pred) || pred == 0 {
		t.Errorf("expected finite non-zero prediction, got %v", pred)
	}

	cloned := mod.Clone()
	if cloned == nil || cloned == mod {
		t.Fatal("expected distinct cloned pointer")
	}
	if got := cloned.Predict(&samples[0].X); math.Abs(got-pred) > 1e-12 {
		t.Errorf("cloned prediction mismatch: got %v, want %v", got, pred)
	}
}

func TestAlmgrenChrissScheduleAndMultiAssetKelly(t *testing.T) {
	totalQty := 12.5
	sched := AlmgrenChrissSchedule(totalQty, 5, 2.0, 0.05, 0.15)
	if len(sched) != 5 {
		t.Fatalf("expected 5 slices, got %d", len(sched))
	}
	var sum float64
	for i, q := range sched {
		if q <= 0 {
			t.Errorf("slice %d non-positive: %v", i, q)
		}
		if i > 0 && q > sched[i-1]+1e-9 {
			t.Errorf("expected front-loaded hyperbolic decay for risk-averse trader: slice[%d]=%v > slice[%d]=%v", i, q, i-1, sched[i-1])
		}
		sum += q
	}
	if math.Abs(sum-totalQty) > 1e-9 {
		t.Errorf("schedule sum = %v, want %v", sum, totalQty)
	}

	// Risk-neutral limit (lambda = 0) should equal TWAP
	twap := AlmgrenChrissSchedule(10.0, 5, 2.0, 0.05, 0.0)
	for i, q := range twap {
		if math.Abs(q-2.0) > 1e-6 {
			t.Errorf("TWAP slice[%d] = %v, want 2.0", i, q)
		}
	}

	// Multi-Asset Kelly Allocator
	edges := [3]float64{2.0, 1.5, 1.8}
	vols := [3]float64{15.0, 20.0, 8.0}
	w := MultiAssetKellyAllocate(edges, vols, 0.65, 0.5, 1.2)
	gross := math.Abs(w[0]) + math.Abs(w[1]) + math.Abs(w[2])
	if gross <= 0 || gross > 1.2+1e-9 {
		t.Errorf("expected gross leverage within (0, 1.2], got %v (w=%v)", gross, w)
	}
	if w[2] <= 0 {
		t.Errorf("expected positive allocation to orthogonal stat-arb leg, got %v", w[2])
	}
}

func BenchmarkPerpLiquidationReversionPredict(b *testing.B) {
	samples := make([]Sample, 100)
	for i := 0; i < 100; i++ {
		var x [FeatureCount]float64
		x[0] = float64(i%7) * 0.1
		x[3] = 0.1
		x[4] = 0.8
		x[7] = -1.5
		x[8] = 0.9
		x[10] = 1.6
		x[11] = -2.1
		x[13] = 0.7
		x[14] = 12.0
		samples[i] = Sample{X: x, Y: x[0] * 1.5}
	}
	mod := NewPerpLiquidationReversionModel(1e-3)
	_ = mod.Fit(samples, []int{0, 3, 4, 7, 8, 10, 11, 13, 14})
	vec := samples[0].X

	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += mod.Predict(&vec)
	}
	if math.IsNaN(sink) {
		b.Fatal("NaN")
	}
}

func BenchmarkMultiAssetKellyAllocate(b *testing.B) {
	edges := [3]float64{1.6, 1.9, 1.4}
	vols := [3]float64{18.0, 24.0, 9.0}
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		w := MultiAssetKellyAllocate(edges, vols, 0.65, 0.5, 1.5)
		sink += w[0] + w[1] + w[2]
	}
	if math.IsNaN(sink) {
		b.Fatal("NaN")
	}
}
