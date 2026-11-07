package common

import (
	"math"
	"testing"
)

func TestTriangularArbModel_ArbitrageCases(t *testing.T) {
	// FeeA = 1.0 bps, FeeB = 1.0 bps, FeeC = 1.0 bps -> Total = 3.0 bps. Threshold = 0.5 bps.
	model := NewTriangularArbModel(1.0, 1.0, 1.0, 1.0, 0.5)

	// Case 1: Direct P_C is overpriced compared to P_synth
	// Leg A (BTCUSDT) = 50,000
	// Leg B (ETHUSDT) = 3,000
	// Synthetic = 3,000 / 50,000 = 0.060000
	// Direct Leg C (ETHBTC) = 0.060100 (divergence = (0.0601 - 0.06) / 0.06 * 10,000 = +16.6667 bps)
	// Net Edge = 16.6667 - 3.0 = 13.6667 bps > 0.5 bps
	d1 := model.UpdatePrices(50000.0, 3000.0, 0.0601)

	if !d1.ArbOpportunity {
		t.Fatalf("expected ArbOpportunity=true, got false")
	}
	if math.Abs(d1.SyntheticPrice-0.06) > 1e-9 {
		t.Errorf("expected SyntheticPrice=0.06, got %f", d1.SyntheticPrice)
	}
	if d1.DirectPrice != 0.0601 {
		t.Errorf("expected DirectPrice=0.0601, got %f", d1.DirectPrice)
	}
	if d1.Leg1Side != "SELL" || d1.Leg2Side != "BUY" || d1.Leg3Side != "SELL" {
		t.Errorf("unexpected leg sides for overpriced direct: Leg1=%s Leg2=%s Leg3=%s", d1.Leg1Side, d1.Leg2Side, d1.Leg3Side)
	}
	if d1.NetEdgeBps <= 0 {
		t.Errorf("expected positive NetEdgeBps, got %f", d1.NetEdgeBps)
	}

	// Case 2: Direct P_C is underpriced compared to P_synth
	// Leg C (ETHBTC) = 0.059900 (divergence = (0.0599 - 0.06) / 0.06 * 10,000 = -16.6667 bps)
	// Net Edge = 16.6667 - 3.0 = 13.6667 bps > 0.5 bps
	d2 := model.UpdatePrices(50000.0, 3000.0, 0.0599)
	if !d2.ArbOpportunity {
		t.Fatalf("expected ArbOpportunity=true, got false")
	}
	if d2.Leg1Side != "BUY" || d2.Leg2Side != "SELL" || d2.Leg3Side != "BUY" {
		t.Errorf("unexpected leg sides for underpriced direct: Leg1=%s Leg2=%s Leg3=%s", d2.Leg1Side, d2.Leg2Side, d2.Leg3Side)
	}

	// Case 3: Negligible divergence within fee hurdle
	// Direct = 0.060010 (divergence ~ 1.6667 bps < 3.0 bps fees)
	d3 := model.UpdatePrices(50000.0, 3000.0, 0.06001)
	if d3.ArbOpportunity {
		t.Errorf("expected ArbOpportunity=false when within fees, got true")
	}
	if d3.Leg1Side != "HOLD" || d3.Leg2Side != "HOLD" || d3.Leg3Side != "HOLD" {
		t.Errorf("expected HOLD on all legs, got Leg1=%s Leg2=%s Leg3=%s", d3.Leg1Side, d3.Leg2Side, d3.Leg3Side)
	}
}

func TestTriangularArbModel_FitPredict(t *testing.T) {
	model := NewTriangularArbModel(1.0, 2.0, 2.0, 2.0, 0.5)
	samples := make([]Sample, 50)
	for i := range samples {
		var x [FeatureCount]float64
		x[3] = float64(i%5 - 2)
		x[4] = 1.5
		x[6] = 0.5
		samples[i] = Sample{X: x, Y: float64(i % 3)}
	}
	if err := model.Fit(samples, nil); err != nil {
		t.Fatalf("Fit failed: %v", err)
	}
	var testX [FeatureCount]float64
	testX[3] = 0.5
	testX[4] = 2.0
	pred := model.Predict(&testX)
	if math.IsNaN(pred) || math.IsInf(pred, 0) {
		t.Fatalf("invalid prediction: %f", pred)
	}
}

func TestJumpDiffusionEstimator_Decomposition(t *testing.T) {
	est := NewJumpDiffusionEstimator()

	// 1. Ingest smooth continuous returns (small gaussian-like increments)
	// Base price 100, varying +/- 0.01%
	price := 100.0
	for i := 0; i < 40; i++ {
		drift := 0.0001
		if i%2 == 0 {
			drift = -0.0001
		}
		price *= (1.0 + drift)
		state := est.AddPrice(price)
		if i > 10 && state.IsJumpActive {
			t.Errorf("step %d: false positive jump detected on continuous regime: %+v", i, state)
		}
	}

	stateCont := est.Compute()
	if stateCont.IsJumpActive {
		t.Errorf("continuous flow flagged as jump active: JR=%f", stateCont.JumpRatio)
	}
	if stateCont.VolMultiplier != 1.0 {
		t.Errorf("expected VolMultiplier=1.0 for continuous flow, got %f", stateCont.VolMultiplier)
	}

	// 2. Introduce a large sudden jump in price (+2.5% shock)
	shockPrice := price * 1.025
	stateJump := est.AddPrice(shockPrice)

	if !stateJump.IsJumpActive {
		t.Fatalf("expected jump active after 2.5%% shock, got state: %+v", stateJump)
	}
	if stateJump.JumpRatio <= 0.40 {
		t.Errorf("expected JumpRatio > 0.40, got %f", stateJump.JumpRatio)
	}
	if stateJump.VolMultiplier <= 1.0 {
		t.Errorf("expected VolMultiplier > 1.0 during jump, got %f", stateJump.VolMultiplier)
	}
	if stateJump.JumpVolBps <= 0 {
		t.Errorf("expected JumpVolBps > 0, got %f", stateJump.JumpVolBps)
	}
}

func BenchmarkTriangularArbModel_Evaluate(b *testing.B) {
	model := NewTriangularArbModel(1.0, 2.0, 2.0, 2.0, 0.5)
	model.PriceA = 65000.0
	model.PriceB = 3500.0
	model.PriceC = 0.0540

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := model.Evaluate()
		if d.SyntheticPrice <= 0 {
			b.Fatal("invalid synth price")
		}
	}
}

func BenchmarkTriangularArbModel_Predict(b *testing.B) {
	model := NewTriangularArbModel(1.0, 2.0, 2.0, 2.0, 0.5)
	var x [FeatureCount]float64
	x[3] = 0.5
	x[4] = 1.2
	x[6] = 0.8

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = model.Predict(&x)
	}
}

func BenchmarkJumpDiffusionEstimator_AddPrice(b *testing.B) {
	est := NewJumpDiffusionEstimator()
	price := 50000.0

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		price += float64(i%10 - 5)
		st := est.AddPrice(price)
		if st.VolMultiplier <= 0 {
			b.Fatal("invalid vol multiplier")
		}
	}
}

func BenchmarkJumpDiffusionEstimator_Compute(b *testing.B) {
	est := NewJumpDiffusionEstimator()
	for i := 0; i < JumpWindowSize; i++ {
		est.AddReturn(float64(i%5-2) * 0.0001)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st := est.Compute()
		if st.RealizedVar < 0 {
			b.Fatal("negative RV")
		}
	}
}
