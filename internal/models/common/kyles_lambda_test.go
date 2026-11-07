package common

import (
	"math"
	"testing"
)

func TestKylesLambda_BasicEstimation(t *testing.T) {
	est := NewKylesLambdaEstimator(0.10, 0.25)

	// Baseline lambda before trades
	if est.Lambda() != 0.25 {
		t.Errorf("expected initial default lambda 0.25, got %f", est.Lambda())
	}

	// Ingest trades with consistent price impact:
	// aggressive buys push price up, aggressive sells push price down
	price := 50000.0
	for i := 0; i < 20; i++ {
		price += 2.5                       // +0.5 bps
		est.UpdateTrade(price, 1.0, false) // buy 1 BTC
		price -= 2.5                       // -0.5 bps
		est.UpdateTrade(price, 1.0, true)  // sell 1 BTC
	}

	lam := est.Lambda()
	if lam <= 0 {
		t.Fatalf("expected positive Kyle's lambda, got %f", lam)
	}
	if lam > 50.0 {
		t.Fatalf("expected bounded lambda <= 50, got %f", lam)
	}
}

func TestKylesLambda_ImpactAndOptimalSize(t *testing.T) {
	est := NewKylesLambdaEstimator(0.05, 0.50)

	// Feed trades to set stable lambda
	price := 50000.0
	for i := 0; i < 10; i++ {
		price += 5.0
		est.UpdateTrade(price, 2.0, false)
		price -= 5.0
		est.UpdateTrade(price, 2.0, true)
	}

	lam := est.Lambda()
	tempImpact, permImpact, totalImpact := est.Impact(2.0)
	if math.Abs(totalImpact-lam*2.0) > 1e-6 {
		t.Errorf("expected total impact %f, got %f", lam*2.0, totalImpact)
	}
	if math.Abs((tempImpact+permImpact)-totalImpact) > 1e-6 {
		t.Errorf("expected temp + perm == total impact")
	}

	// Sizing policy: optimal size for 2.0 bps impact budget
	budgetBps := 2.0
	maxSize := est.OptimalSize(budgetBps, price)
	expectedSize := budgetBps / lam
	if math.Abs(maxSize-expectedSize) > 1e-6 {
		t.Errorf("expected optimal size %f, got %f", expectedSize, maxSize)
	}
	// Verify that maxSize produces impact <= budgetBps
	_, _, actualImpact := est.Impact(maxSize)
	if math.Abs(actualImpact-budgetBps) > 1e-6 {
		t.Errorf("expected impact at optimal size %f to equal budget %f", actualImpact, budgetBps)
	}
}

func TestKylesLambdaModel(t *testing.T) {
	mod := NewKylesLambdaModel(1e-3, 0.05)

	samples := make([]Sample, 50)
	for i := range samples {
		var x [FeatureCount]float64
		x[0] = 0.3
		x[4] = 0.8
		x[5] = 1.0
		samples[i] = Sample{
			X: x,
			Y: 0.6,
		}
	}

	if err := mod.Fit(samples, nil); err != nil {
		t.Fatalf("Fit failed: %v", err)
	}

	var xLowImpact [FeatureCount]float64
	xLowImpact[5] = 2.0
	xLowImpact[11] = 0.1 // small price move with large flow => low lambda
	predLow := mod.Predict(&xLowImpact)

	var xHighImpact [FeatureCount]float64
	xHighImpact[5] = 0.1
	xHighImpact[11] = 4.0 // huge price move with tiny flow => huge lambda
	predHigh := mod.Predict(&xHighImpact)

	// Conviction should be dampened under high lambda
	if math.Abs(predHigh) >= math.Abs(predLow) && predLow != 0 {
		t.Errorf("expected high lambda to dampen alpha forecast: low=%f, high=%f", predLow, predHigh)
	}
}

func BenchmarkKylesLambda_UpdateTrade(b *testing.B) {
	est := NewKylesLambdaEstimator(0.05, 0.25)
	b.ReportAllocs()
	b.ResetTimer()
	p := 50000.0
	for i := 0; i < b.N; i++ {
		p += float64((i % 3) - 1)
		est.UpdateTrade(p, 1.5, i%2 == 0)
	}
}

func BenchmarkKylesLambda_Lambda(b *testing.B) {
	est := NewKylesLambdaEstimator(0.05, 0.25)
	p := 50000.0
	for i := 0; i < 50; i++ {
		p += float64((i % 3) - 1)
		est.UpdateTrade(p, 1.5, i%2 == 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = est.Lambda()
	}
}

func BenchmarkKylesLambda_OptimalSize(b *testing.B) {
	est := NewKylesLambdaEstimator(0.05, 0.25)
	p := 50000.0
	for i := 0; i < 50; i++ {
		p += float64((i % 3) - 1)
		est.UpdateTrade(p, 1.5, i%2 == 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = est.OptimalSize(1.5, 50000.0)
	}
}

func BenchmarkKylesLambda_Impact(b *testing.B) {
	est := NewKylesLambdaEstimator(0.05, 0.25)
	p := 50000.0
	for i := 0; i < 50; i++ {
		p += float64((i % 3) - 1)
		est.UpdateTrade(p, 1.5, i%2 == 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = est.Impact(2.5)
	}
}
