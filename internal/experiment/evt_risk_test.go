package experiment

import (
	"math"
	"math/rand"
	"testing"
)

func TestEVTRiskEngineFitPOT(t *testing.T) {
	// Generate 1000 simulated losses with a heavy-tailed GPD upper tail (u = 2.0, xi = 0.2, beta = 1.5)
	r := rand.New(rand.NewSource(42))
	losses := make([]float64, 1000)
	u := 2.0
	xiTrue := 0.2
	betaTrue := 1.5

	for i := 0; i < 1000; i++ {
		if i < 800 {
			// Body of distribution: normal/exponential below u
			losses[i] = r.Float64() * u
		} else {
			// Tail: GPD above u
			p := r.Float64()
			y := (betaTrue / xiTrue) * (math.Pow(1.0-p, -xiTrue) - 1.0)
			losses[i] = u + y
		}
	}

	engine := NewEVTRiskEngine()
	if err := engine.FitPOT(losses, u); err != nil {
		t.Fatalf("FitPOT failed: %v", err)
	}

	p := engine.Params
	if p.Nu != 200 {
		t.Fatalf("expected 200 exceedances, got %d", p.Nu)
	}

	// Verify fitted parameters are reasonably close to ground truth
	if math.Abs(p.Xi-xiTrue) > 0.15 {
		t.Errorf("expected xi ~ %f, got %f", xiTrue, p.Xi)
	}
	if math.Abs(p.Beta-betaTrue) > 0.35 {
		t.Errorf("expected beta ~ %f, got %f", betaTrue, p.Beta)
	}

	// Compute 99% VaR (alpha = 0.01)
	var99 := engine.VaR(0.01)
	if var99 <= u {
		t.Errorf("expected VaR_0.99 > threshold %f, got %f", u, var99)
	}

	// Compute 99% Expected Shortfall
	es99 := engine.ExpectedShortfall(0.01)
	if es99 <= var99 {
		t.Errorf("expected ES_0.99 (%f) > VaR_0.99 (%f)", es99, var99)
	}
}

func TestMicrostructureCircuitBreaker(t *testing.T) {
	cb := NewMicrostructureCircuitBreaker(1.0) // median spread = 1.0

	// 1. Normal conditions
	halted, reason := cb.CheckWithTime(1.0, 100.0, 0.2, 100_000_000)
	if halted || cb.State != StateNormal {
		t.Errorf("expected Normal state, got halted=%v, state=%v", halted, cb.State)
	}

	// 2. Spread blowout: spread jumps to 5.5x median spread
	halted, reason = cb.CheckWithTime(5.5, 100.0, 0.2, 200_000_000)
	if !halted || reason != "spread_blowout_5x" || cb.State != StateHalted {
		t.Errorf("expected halted on spread blowout, got halted=%v, reason=%q", halted, reason)
	}

	// Reset
	cb.Reset(1.0)

	// 3. VPIN spike: vpin exceeds 0.85
	halted, reason = cb.CheckWithTime(1.0, 100.0, 0.88, 300_000_000)
	if !halted || reason != "vpin_spike_85" || cb.State != StateHalted {
		t.Errorf("expected halted on VPIN spike, got halted=%v, reason=%q", halted, reason)
	}

	// Reset
	cb.Reset(1.0)

	// 4. Top-5 depth collapse: drops from 100 to 15 (85% drop) within 300ms
	cb.CheckWithTime(1.0, 100.0, 0.2, 400_000_000)
	halted, reason = cb.CheckWithTime(1.0, 15.0, 0.2, 600_000_000)
	if !halted || reason != "depth_collapse_80pct" || cb.State != StateHalted {
		t.Errorf("expected halted on depth collapse, got halted=%v, reason=%q", halted, reason)
	}
}

func BenchmarkEVTRiskEngine(b *testing.B) {
	engine := NewEVTRiskEngine()
	engine.Params = GPDParams{
		Threshold: 2.0,
		Xi:        0.2,
		Beta:      1.5,
		Nu:        200,
		N:         1000,
	}
	engine.fitted = true

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = engine.VaR(0.01)
		_ = engine.ExpectedShortfall(0.01)
	}
}

func BenchmarkMicrostructureCircuitBreaker(b *testing.B) {
	cb := NewMicrostructureCircuitBreaker(1.0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cb.Check(1.2, 95.0, 0.35)
	}
}
