package common

import (
	"math"
	"testing"
)

func TestSmartOrderRouterRegimesAndVenues(t *testing.T) {
	sor := NewSmartOrderRouter()

	// 1. Calm regime, shallow queue ahead, moderate alpha, negative perp basis (perp discount -> favorable for buy):
	// Should route passively via MakerPerp
	dMaker := sor.Route(1.20, 0.15, 1.0, 1.0, -1.50)
	if dMaker.Action != RouteMakerPerp {
		t.Errorf("expected RouteMakerPerp for shallow queue and discount perp basis, got %s (theta=%.2f, perp=%.2f)",
			dMaker.Action, dMaker.MakerFraction, dMaker.PerpAllocation)
	}
	if dMaker.MakerFraction != 1.0 || dMaker.TakerFraction != 0.0 {
		t.Errorf("expected MakerFraction=1.0, got %.2f", dMaker.MakerFraction)
	}
	if dMaker.PrimaryVenue != "BINANCE_PERP" {
		t.Errorf("expected BINANCE_PERP primary venue, got %s", dMaker.PrimaryVenue)
	}
	if dMaker.ExpectedEdgeBps <= 0 {
		t.Errorf("expected positive net edge, got %.4f", dMaker.ExpectedEdgeBps)
	}

	// 2. High-urgency alpha with deep queue ahead (low P(fill)) -> should route as Taker or SplitMakerTaker
	dUrgent := sor.Route(18.0, 0.10, 120.0, 1.2, -1.0)
	if dUrgent.Action != RouteTakerPerp && dUrgent.Action != RouteSplitMakerTaker {
		t.Errorf("expected urgent deep-queue order to use TakerPerp or SplitMakerTaker, got %s (theta=%.2f)",
			dUrgent.Action, dUrgent.MakerFraction)
	}
	if dUrgent.TakerFraction <= 0.15 {
		t.Errorf("expected significant TakerFraction > 0.15 on urgent order, got %.2f", dUrgent.TakerFraction)
	}
	if dUrgent.ExpectedSlippageBps <= 0 {
		t.Errorf("expected positive slippage drag when taking liquidity, got %.4f", dUrgent.ExpectedSlippageBps)
	}

	// 3. Interior Maker/Taker split when alpha is medium-high and queue is moderately congested
	dSplit := sor.Route(6.5, 0.25, 28.0, 1.5, 0.0)
	if dSplit.Action != RouteSplitMakerTaker {
		t.Errorf("expected RouteSplitMakerTaker for balanced urgency/queue, got %s (theta=%.2f)",
			dSplit.Action, dSplit.MakerFraction)
	}
	if dSplit.MakerFraction <= 0.15 || dSplit.MakerFraction >= 0.85 {
		t.Errorf("expected interior MakerFraction in (0.15, 0.85), got %.3f", dSplit.MakerFraction)
	}
	if math.Abs((dSplit.MakerFraction+dSplit.TakerFraction)-1.0) > 1e-9 {
		t.Errorf("expected MakerFraction + TakerFraction == 1.0")
	}

	// 4. Strongly adverse perp basis for a buy order (perp trading at steep premium +8 bps) -> shifts toward Spot
	dSpot := sor.Route(1.50, 0.10, 0.8, 1.0, 8.50)
	if dSpot.PrimaryVenue != "BINANCE_SPOT" || dSpot.Action != RouteMakerSpot {
		t.Errorf("expected BINANCE_SPOT / RouteMakerSpot when perp trades at steep premium for buy, got venue=%s action=%s",
			dSpot.PrimaryVenue, dSpot.Action)
	}
}

func TestESNReservoirModelFitPredictClone(t *testing.T) {
	n := 180
	samples := make([]Sample, n)
	for i := 0; i < n; i++ {
		var x [FeatureCount]float64
		tVal := float64(i) * 0.12
		x[0] = math.Sin(tVal)
		x[1] = math.Sin(tVal - 0.25)
		x[2] = math.Cos(tVal)
		x[3] = 0.35 * x[0]
		x[4] = 0.85
		x[5] = 1.8 * math.Sin(tVal)
		x[8] = 1.4 * math.Cos(tVal)
		x[10] = 1.5
		x[11] = 0.9 * x[0]
		x[13] = 0.7 * x[5]
		// Non-linear target combining direct linear trend and tanh reservoir interaction
		y := 1.75*x[0] + 0.65*x[5] + 0.85*math.Tanh(x[0]*x[8]-0.5*x[1])
		samples[i] = Sample{X: x, Y: y, Vol: 1.5}
	}

	esn := NewESNReservoirModel(1e-3)
	if esn.Name() != "esn_reservoir" {
		t.Fatalf("expected Name() = esn_reservoir, got %q", esn.Name())
	}

	if err := esn.Fit(samples); err != nil {
		t.Fatalf("ESNReservoirModel.Fit failed: %v", err)
	}

	p1 := esn.Predict(&samples[10].X)
	p2 := esn.PredictSlice(samples[10].X[:])
	if !Finite(p1) || p1 == 0 {
		t.Fatalf("expected finite non-zero ESN prediction, got %v", p1)
	}
	if math.Abs(p1-p2) > 1e-12 {
		t.Errorf("Predict vs PredictSlice mismatch: %v vs %v", p1, p2)
	}

	// Verify Clone produces an independent copy with identical predictions
	cloned := esn.Clone()
	if cloned == nil || cloned == esn {
		t.Fatal("expected distinct non-nil cloned ESNReservoirModel pointer")
	}
	if got := cloned.Predict(&samples[10].X); math.Abs(got-p1) > 1e-12 {
		t.Errorf("cloned prediction mismatch: got %v, want %v", got, p1)
	}
}

func BenchmarkSmartOrderRouterRoute(b *testing.B) {
	sor := NewSmartOrderRouter()
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		dec := sor.Route(2.45, 0.20, 12.5, 1.65, -0.80)
		sink += dec.ExpectedEdgeBps + dec.MakerFraction
	}
	if math.IsNaN(sink) {
		b.Fatal("NaN")
	}
}

func BenchmarkESNReservoirModelPredict(b *testing.B) {
	samples := make([]Sample, 100)
	for i := 0; i < 100; i++ {
		var x [FeatureCount]float64
		x[0] = float64(i%7) * 0.12
		x[1] = float64(i%5) * 0.09
		x[3] = 0.14
		x[4] = 0.85
		x[5] = 1.35
		x[8] = 1.05
		x[10] = 1.50
		x[11] = 0.75
		x[13] = 0.55
		samples[i] = Sample{X: x, Y: 1.6*x[0] + 0.5*math.Tanh(x[5])}
	}
	esn := NewESNReservoirModel(1e-3)
	_ = esn.Fit(samples)
	vec := samples[0].X

	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += esn.Predict(&vec)
	}
	if math.IsNaN(sink) {
		b.Fatal("NaN")
	}
}
