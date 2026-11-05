package common

import (
	"math"
	"testing"
)

func TestVPINCalculator(t *testing.T) {
	calc := NewVPINCalculator(10.0, 10)

	// Ingest 50 units of aggressive buy volume: 5 buckets filled with 10 buy each
	calc.Update(100.0, 50.0, false) // isBuyerMaker = false => Buy

	if calc.Filled != 5 {
		t.Fatalf("expected 5 filled buckets, got %d", calc.Filled)
	}

	// 5 buckets of pure buy: |10 - 0| = 10 each. Total diff = 50. Total vol = 50. VPIN = 1.0
	vpin := calc.VPIN()
	if math.Abs(vpin-1.0) > 1e-6 {
		t.Errorf("expected VPIN 1.0, got %f", vpin)
	}

	if !calc.IsToxic(0.8) {
		t.Errorf("expected IsToxic(0.8) = true")
	}

	// Ingest 50 units of aggressive sell volume: 5 buckets of 10 sell each
	calc.Update(100.0, 50.0, true) // isBuyerMaker = true => Sell
	if calc.Filled != 10 {
		t.Fatalf("expected 10 filled buckets, got %d", calc.Filled)
	}

	// Now 10 buckets total: 5 pure buy, 5 pure sell. |10 - 0| = 10 each. VPIN is still 1.0
	vpin = calc.VPIN()
	if math.Abs(vpin-1.0) > 1e-6 {
		t.Errorf("expected VPIN 1.0, got %f", vpin)
	}

	// Now add balanced flow: 100 units of alternating 5 buy, 5 sell in same bucket
	calc.Reset()
	for i := 0; i < 10; i++ {
		calc.Update(100.0, 5.0, false)
		calc.Update(100.0, 5.0, true)
	}
	if calc.Filled != 10 {
		t.Fatalf("expected 10 filled buckets, got %d", calc.Filled)
	}
	// In each bucket: 5 buy, 5 sell => |5 - 5| = 0. VPIN = 0.0
	vpin = calc.VPIN()
	if math.Abs(vpin-0.0) > 1e-6 {
		t.Errorf("expected VPIN 0.0 for balanced flow, got %f", vpin)
	}
	if calc.IsToxic(0.5) {
		t.Errorf("expected IsToxic(0.5) = false for balanced flow")
	}
}

func TestContDeLarrardQueue(t *testing.T) {
	// 10 units ahead, cancel rate = 2/s, trade rate = 3/s, OFI = 0. Total depletion = 5/s
	q := NewContDeLarrardQueue(10.0, 2.0, 3.0, 0.0)

	expectedT := q.ExpectedTimeToFill()
	if math.Abs(expectedT-2.0) > 1e-6 {
		t.Errorf("expected time to fill 2.0s, got %f", expectedT)
	}

	// P(fill within 2s): 1 - exp(- (5/10) * 2) = 1 - exp(-1) = 0.63212
	p := q.FillProbability(2.0)
	if math.Abs(p-(1.0-math.Exp(-1.0))) > 1e-4 {
		t.Errorf("expected fill prob %f, got %f", 1.0-math.Exp(-1.0), p)
	}

	// If q_ahead is 0, prob is 1.0 and expected time is 0
	qZero := NewContDeLarrardQueue(0.0, 2.0, 3.0, 0.0)
	if qZero.FillProbability(1.0) != 1.0 {
		t.Errorf("expected 1.0 fill prob for 0 queue ahead")
	}
	if qZero.ExpectedTimeToFill() != 0 {
		t.Errorf("expected 0 expected time for 0 queue ahead")
	}
}

func TestCarteaJaimungalMM(t *testing.T) {
	mm := NewCarteaJaimungalMM(0.05, 1.5, 2.0, 0.001, 0.01, 0.5, 60.0)

	mid := 100.0
	// 1. Zero inventory, zero OFI
	bid, ask, res := mm.ComputeQuotes(mid, 0, 0, 60.0, 0)
	if math.Abs(res-mid) > 1e-6 {
		t.Errorf("expected reservation price %f, got %f", mid, res)
	}
	if ask <= bid {
		t.Errorf("expected ask > bid, got ask=%f, bid=%f", ask, bid)
	}

	// 2. Positive inventory (long 5 units): reservation price should skew down
	bidLong, askLong, resLong := mm.ComputeQuotes(mid, 5.0, 0, 60.0, 0)
	if resLong >= res {
		t.Errorf("expected long inventory to depress reservation price, got resLong=%f >= res=%f", resLong, res)
	}
	if askLong >= ask {
		t.Errorf("expected long inventory to lower ask quote to unload inventory, got askLong=%f >= ask=%f", askLong, ask)
	}
	if bidLong >= bid {
		t.Errorf("expected long inventory to lower bid quote, got bidLong=%f >= bid=%f", bidLong, bid)
	}

	// 3. Positive OFI (buying pressure): reservation price should skew down by rho * OFI
	_, _, resOFI := mm.ComputeQuotes(mid, 0, 0, 60.0, 10.0)
	expectedResOFI := mid - 0.5*10.0
	if math.Abs(resOFI-expectedResOFI) > 1e-6 {
		t.Errorf("expected resOFI %f, got %f", expectedResOFI, resOFI)
	}
}

func BenchmarkVPINCalculatorUpdate(b *testing.B) {
	calc := NewVPINCalculator(10.0, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		calc.Update(100.0, 2.5, i%2 == 0)
	}
}

func BenchmarkContDeLarrardQueue(b *testing.B) {
	q := NewContDeLarrardQueue(10.0, 1.5, 2.5, -0.5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = q.FillProbability(5.0)
		_ = q.ExpectedTimeToFill()
	}
}

func BenchmarkCarteaJaimungalMM(b *testing.B) {
	mm := NewCarteaJaimungalMM(0.05, 1.5, 2.0, 0.001, 0.01, 0.5, 60.0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = mm.ComputeQuotes(100.0, 2.0, 0.0, 30.0, 1.5)
	}
}
