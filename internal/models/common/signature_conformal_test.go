package common

import (
	"math"
	"testing"
)

func TestSplitConformalCalibratorAndGate(t *testing.T) {
	n := 120
	samples := make([]Sample, n)
	preds := make([]float64, n)
	for i := 0; i < n; i++ {
		vol := 1.0 + float64(i%3)*1.0 // 1.0 (calm), 2.0 (normal), 3.0 (high)
		pred := math.Sin(float64(i)*0.2) * 3.0
		// Residual roughly proportional to vol
		err := (float64((i*7)%11) - 5.0) * 0.25 * vol
		var x [FeatureCount]float64
		x[10] = vol
		samples[i] = Sample{X: x, Y: pred + err, Vol: vol}
		preds[i] = pred
	}

	cal := NewSplitConformalCalibrator(0.10)
	cal.Calibrate(samples, preds)

	if !cal.Calibrated || cal.Quantile <= 0 {
		t.Fatalf("expected calibrated positive quantile, got Calibrated=%v Quantile=%v", cal.Calibrated, cal.Quantile)
	}

	// Verify empirical coverage on calibration set >= 90%
	var covered int
	for i := 0; i < n; i++ {
		lo, hi, cert := cal.Interval(preds[i], samples[i].Vol)
		if lo >= hi {
			t.Fatalf("expected lower < upper, got [%v, %v]", lo, hi)
		}
		if cert < 0 || cert > 1.0 {
			t.Fatalf("expected certitude in [0, 1], got %v", cert)
		}
		if samples[i].Y >= lo-1e-9 && samples[i].Y <= hi+1e-9 {
			covered++
		}
	}
	covRate := float64(covered) / float64(n)
	if covRate < 0.88 {
		t.Errorf("expected empirical 90%% conformal coverage >= 0.88, got %.3f", covRate)
	}

	// Strong forecast should pass conformal gate; tiny forecast should fail
	if !cal.PassesConformalGate(5.5, 1.0, 0.20) {
		t.Errorf("expected strong +5.5 bps forecast in low vol to pass conformal gate")
	}
	if !cal.PassesConformalGate(-5.5, 1.0, 0.20) {
		t.Errorf("expected strong -5.5 bps forecast in low vol to pass conformal gate")
	}
	if cal.PassesConformalGate(0.05, 2.5, 0.30) {
		t.Errorf("expected tiny +0.05 bps forecast in high vol to fail conformal gate")
	}
}

func TestComputeLevyAreaFeaturesAndRoughSignatureOFIModel(t *testing.T) {
	var x [FeatureCount]float64
	x[0] = 0.45  // imb_l1
	x[1] = 0.20  // imb_l5
	x[2] = -0.10 // imb_l20
	x[3] = 0.25  // micro_dev
	x[4] = 1.0   // spread_bps
	x[5] = 2.4   // ofi_1s
	x[8] = 1.8   // mlofi_1s
	x[11] = 1.6  // ret_1s_bps / Hawkes velocity
	x[13] = 1.2  // trade_imb_5s

	sig := ComputeLevyAreaFeatures(x)
	for k, v := range sig {
		if !Finite(v) {
			t.Fatalf("signature feature %d is non-finite: %v", k, v)
		}
	}
	if sig[0] == 0 && sig[1] == 0 && sig[5] == 0 {
		t.Errorf("expected non-zero Lévy area and Kyle impact terms, got %v", sig)
	}

	// Train RoughSignatureOFIModel on synthetic data with rotational lead-lag signature alpha
	samples := make([]Sample, 180)
	for i := 0; i < 180; i++ {
		var sx [FeatureCount]float64
		tVal := float64(i) * 0.15
		sx[0] = math.Sin(tVal)
		sx[1] = math.Sin(tVal - 0.3)
		sx[2] = math.Cos(tVal)
		sx[3] = 0.3 * sx[0]
		sx[4] = 0.9
		sx[5] = 2.0 * math.Sin(tVal)
		sx[8] = 1.5 * math.Cos(tVal)
		sx[10] = 1.8
		sx[11] = 1.2 * sx[0]
		sx[13] = 0.9 * sx[5]
		sTerms := ComputeLevyAreaFeatures(sx)
		y := 1.6*sx[0] + 0.8*sx[5] + 0.9*sTerms[0] + 0.6*sTerms[3]
		samples[i] = Sample{X: sx, Y: y, Vol: 1.8}
	}

	mod := NewRoughSignatureOFIModel(1e-3)
	if mod.Name() != "rough_signature_ofi" {
		t.Fatalf("expected Name() = rough_signature_ofi, got %q", mod.Name())
	}
	if err := mod.Fit(samples, nil); err != nil {
		t.Fatalf("RoughSignatureOFIModel.Fit failed: %v", err)
	}

	p1 := mod.Predict(&samples[5].X)
	p2 := mod.PredictSlice(samples[5].X[:])
	if !Finite(p1) || p1 == 0 {
		t.Errorf("expected finite non-zero prediction, got %v", p1)
	}
	if math.Abs(p1-p2) > 1e-12 {
		t.Errorf("Predict vs PredictSlice mismatch: %v vs %v", p1, p2)
	}

	clone := mod.Clone()
	if clone == nil || clone == mod {
		t.Fatal("expected distinct cloned RoughSignatureOFIModel pointer")
	}
	if got := clone.Predict(&samples[5].X); math.Abs(got-p1) > 1e-12 {
		t.Errorf("cloned prediction mismatch: got %v, want %v", got, p1)
	}
}

func TestAdaHedgeEnsembleRegretBoundedAdaptation(t *testing.T) {
	ens := NewAdaHedgeEnsemble(6, 0.98)

	// Simulate 80 steps where expert 5 (rough_signature_ofi) is consistently accurate
	// while other experts have larger errors.
	for step := 0; step < 80; step++ {
		target := 2.0 * math.Sin(float64(step)*0.2)
		prevPreds := [MaxEnsembleExperts]float64{
			target + 3.0,  // expert 0: noisy
			-target,       // expert 1: inverted
			0.0,           // expert 2: zero
			target + 2.2,  // expert 3: biased
			target - 2.5,  // expert 4: biased
			target + 0.05, // expert 5: accurate!
		}
		currPreds := prevPreds
		comb, weights := ens.UpdateAndCombine(target, prevPreds, currPreds)
		if !Finite(comb) {
			t.Fatalf("non-finite AdaHedge combined output at step %d: %v", step, comb)
		}
		var sumW float64
		for k := 0; k < 6; k++ {
			sumW += weights[k]
		}
		if math.Abs(sumW-1.0) > 1e-9 {
			t.Fatalf("weights do not sum to 1.0: got %v", sumW)
		}
	}

	// Best expert (index 5) must hold the highest weight
	bestW := ens.Weights[5]
	for k := 0; k < 5; k++ {
		if ens.Weights[k] >= bestW {
			t.Errorf("expected expert 5 weight (%v) > expert %d weight (%v)", bestW, k, ens.Weights[k])
		}
	}
	if bestW < 0.35 {
		t.Errorf("expected AdaHedge to concentrate weight >= 0.35 on best expert, got %v", bestW)
	}
}

func BenchmarkRoughSignatureOFIModelPredict(b *testing.B) {
	samples := make([]Sample, 100)
	for i := 0; i < 100; i++ {
		var x [FeatureCount]float64
		x[0] = float64(i%7) * 0.1
		x[1] = float64(i%5) * 0.08
		x[2] = -0.05
		x[3] = 0.15
		x[4] = 0.8
		x[5] = 1.4
		x[8] = 1.1
		x[11] = 0.9
		x[13] = 0.6
		samples[i] = Sample{X: x, Y: x[0]*1.8 + 0.4}
	}
	mod := NewRoughSignatureOFIModel(1e-3)
	_ = mod.Fit(samples, nil)
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

func BenchmarkSplitConformalCalibratorInterval(b *testing.B) {
	cal := NewSplitConformalCalibrator(0.10)
	cal.Calibrated = true
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		lo, hi, cert := cal.Interval(1.85, 1.40)
		if cal.PassesConformalGate(1.85, 1.40, 0.20) {
			sink += hi - lo + cert
		}
	}
	if math.IsNaN(sink) {
		b.Fatal("NaN")
	}
}

func BenchmarkAdaHedgeEnsembleCombine(b *testing.B) {
	ens := NewAdaHedgeEnsemble(8, 0.985)
	prev := [MaxEnsembleExperts]float64{0.5, 0.4, 0.6, 0.3, 0.55, 0.48, 0.52, 0.45}
	curr := [MaxEnsembleExperts]float64{0.6, 0.5, 0.7, 0.4, 0.65, 0.58, 0.62, 0.55}

	b.Run("Combine", func(b *testing.B) {
		b.ReportAllocs()
		var sink float64
		for i := 0; i < b.N; i++ {
			sink += ens.Combine(&curr)
		}
		if math.IsNaN(sink) {
			b.Fatal("NaN")
		}
	})

	b.Run("UpdateAndCombine", func(b *testing.B) {
		b.ReportAllocs()
		var sink float64
		for i := 0; i < b.N; i++ {
			c, w := ens.UpdateAndCombine(0.55, prev, curr)
			sink += c + w[0]
		}
		if math.IsNaN(sink) {
			b.Fatal("NaN")
		}
	})
}
