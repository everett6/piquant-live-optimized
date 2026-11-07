package common

import (
	"math"
	"testing"
)

func TestVPIN_BasicToxicity(t *testing.T) {
	calc := NewVPINCalculator(5.0, 10)

	// Ingest 50 units of buy volume: fills 10 buckets with 5 units of buy each
	for i := 0; i < 10; i++ {
		calc.UpdateTrade(50000.0+float64(i), 5.0, false) // isBuyerMaker = false => Buy
	}

	if calc.Filled != 10 {
		t.Fatalf("expected 10 filled buckets, got %d", calc.Filled)
	}

	// 10 buckets of pure buy: |5 - 0| = 5 each. VPIN = 10*5 / (10*5) = 1.0
	vpin := calc.VPIN()
	if math.Abs(vpin-1.0) > 1e-6 {
		t.Errorf("expected VPIN 1.0, got %f", vpin)
	}

	if !calc.IsToxic() {
		t.Errorf("expected IsToxic() = true for VPIN 1.0")
	}
	if !calc.IsToxic(0.8) {
		t.Errorf("expected IsToxic(0.8) = true for VPIN 1.0")
	}

	// Ingest balanced flow into a fresh calculator
	calc.Reset()
	for i := 0; i < 10; i++ {
		calc.UpdateTrade(50000.0, 2.5, false) // 2.5 Buy
		calc.UpdateTrade(50000.0, 2.5, true)  // 2.5 Sell => bucket complete with |2.5 - 2.5| = 0
	}
	if calc.Filled != 10 {
		t.Fatalf("expected 10 filled buckets, got %d", calc.Filled)
	}
	vpin = calc.VPIN()
	if math.Abs(vpin-0.0) > 1e-6 {
		t.Errorf("expected VPIN 0.0 for balanced flow, got %f", vpin)
	}
	if calc.IsToxic() {
		t.Errorf("expected IsToxic() = false for balanced flow")
	}
}

func TestVPIN_TickRuleAndBVC(t *testing.T) {
	calc := NewVPINCalculator(5.0, 10)

	// Tick rule: upticks => buys, downticks => sells
	calc.UpdateTradeTick(100.0, 0)   // initial price reference
	calc.UpdateTradeTick(101.0, 5.0) // uptick => buy bucket filled
	if calc.Filled != 1 {
		t.Fatalf("expected 1 filled bucket, got %d", calc.Filled)
	}
	if math.Abs(calc.VPIN()-1.0) > 1e-6 {
		t.Errorf("expected VPIN 1.0 on pure uptick, got %f", calc.VPIN())
	}

	calc.UpdateTradeTick(99.0, 5.0) // downtick => sell bucket filled
	if calc.Filled != 2 {
		t.Fatalf("expected 2 filled buckets, got %d", calc.Filled)
	}

	// BVC (Bulk Volume Classification)
	calc.Reset()
	calc.UpdateTradeBVC(100.0, 0, 1.0) // initial reference price
	// Large positive price jump => bulk classified primarily as buy
	calc.UpdateTradeBVC(105.0, 5.0, 1.0)
	if calc.Filled < 1 {
		t.Fatalf("expected at least 1 filled bucket via BVC, got %d", calc.Filled)
	}
	if calc.VPIN() < 0.90 {
		t.Errorf("expected high VPIN on strong upward BVC trade, got %f", calc.VPIN())
	}
}

func TestVPIN_CDF(t *testing.T) {
	calc := NewVPINCalculator(5.0, 10)

	// Generate a distribution of buckets
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			calc.UpdateTrade(50000.0, 5.0, false) // 100% toxic buy
		} else {
			calc.UpdateTrade(50000.0, 2.5, false)
			calc.UpdateTrade(50000.0, 2.5, true) // 0% toxic balanced
		}
	}

	cdf := calc.CDF()
	if cdf < 0.0 || cdf > 1.0 {
		t.Errorf("expected CDF in [0, 1], got %f", cdf)
	}
}

func TestVPIN_AdjustQuote(t *testing.T) {
	calc := NewVPINCalculator(5.0, 10)
	calc.ToxicThreshold = 0.70
	calc.BasePenaltyBps = 5.0

	bid := 50000.0
	ask := 50001.0

	// 1. Balanced flow: VPIN = 0, no widening
	for i := 0; i < 10; i++ {
		calc.UpdateTrade(50000.0, 2.5, false)
		calc.UpdateTrade(50000.0, 2.5, true)
	}
	newBid, newAsk, widenedBps := calc.AdjustQuote(bid, ask)
	if widenedBps != 0 {
		t.Errorf("expected widenedBps == 0 for balanced flow, got %f", widenedBps)
	}
	if newBid != bid || newAsk != ask {
		t.Errorf("expected quotes unchanged, got bid=%f, ask=%f", newBid, newAsk)
	}

	// 2. Toxic flow: VPIN = 1.0, quote should widen symmetrically
	calc.Reset()
	for i := 0; i < 10; i++ {
		calc.UpdateTrade(50000.0, 5.0, false)
	}
	newBid, newAsk, widenedBps = calc.AdjustQuote(bid, ask)
	if widenedBps < 5.0 {
		t.Errorf("expected widenedBps >= 5.0 for toxic flow, got %f", widenedBps)
	}
	if newBid >= bid {
		t.Errorf("expected newBid (%f) < oldBid (%f)", newBid, bid)
	}
	if newAsk <= ask {
		t.Errorf("expected newAsk (%f) > oldAsk (%f)", newAsk, ask)
	}
	if (newAsk - newBid) <= (ask - bid) {
		t.Errorf("expected spread to widen under toxic flow")
	}
}

func TestVPINToxicityModel(t *testing.T) {
	mod := NewVPINToxicityModel(1e-3, 0.70)

	samples := make([]Sample, 50)
	for i := range samples {
		var x [FeatureCount]float64
		x[0] = 0.2
		x[4] = 1.0
		x[5] = 0.5
		x[13] = 0.1
		x[14] = 10.0
		samples[i] = Sample{
			X: x,
			Y: 0.5,
		}
	}

	if err := mod.Fit(samples, nil); err != nil {
		t.Fatalf("Fit failed: %v", err)
	}

	var xNormal [FeatureCount]float64
	xNormal[4] = 1.0
	xNormal[5] = 0.5
	xNormal[13] = 0.1
	xNormal[14] = 5.0
	predNormal := mod.Predict(&xNormal)

	// Extreme toxic condition: high spread, high trade imbalance, high frequency
	var xToxic [FeatureCount]float64
	xToxic[4] = 6.0
	xToxic[5] = 0.5
	xToxic[13] = 0.95
	xToxic[14] = 40.0
	predToxic := mod.Predict(&xToxic)

	// Alpha under toxic flow should be dampened
	if math.Abs(predToxic) >= math.Abs(predNormal) && predNormal != 0 {
		t.Errorf("expected toxic flow to dampen alpha forecast: normal=%f, toxic=%f", predNormal, predToxic)
	}
}

func BenchmarkVPIN_UpdateTrade(b *testing.B) {
	calc := NewVPINCalculator(5.0, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		calc.UpdateTrade(50000.0, 1.25, i%2 == 0)
	}
}

func BenchmarkVPIN_VPIN(b *testing.B) {
	calc := NewVPINCalculator(5.0, 50)
	for i := 0; i < 50; i++ {
		calc.UpdateTrade(50000.0, 5.0, i%2 == 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = calc.VPIN()
	}
}

func BenchmarkVPIN_AdjustQuote(b *testing.B) {
	calc := NewVPINCalculator(5.0, 50)
	for i := 0; i < 50; i++ {
		calc.UpdateTrade(50000.0, 5.0, false)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = calc.AdjustQuote(50000.0, 50001.0)
	}
}

func BenchmarkVPIN_CDF(b *testing.B) {
	calc := NewVPINCalculator(5.0, 50)
	for i := 0; i < 50; i++ {
		calc.UpdateTrade(50000.0, 5.0, i%3 == 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = calc.CDF()
	}
}

func BenchmarkVPIN_UpdateTradeTick(b *testing.B) {
	calc := NewVPINCalculator(5.0, 50)
	b.ReportAllocs()
	b.ResetTimer()
	p := 50000.0
	for i := 0; i < b.N; i++ {
		p += float64((i % 3) - 1)
		calc.UpdateTradeTick(p, 1.25)
	}
}

func BenchmarkVPIN_UpdateTradeBVC(b *testing.B) {
	calc := NewVPINCalculator(5.0, 50)
	b.ReportAllocs()
	b.ResetTimer()
	p := 50000.0
	for i := 0; i < b.N; i++ {
		p += float64((i % 3) - 1)
		calc.UpdateTradeBVC(p, 1.25, 2.0)
	}
}
