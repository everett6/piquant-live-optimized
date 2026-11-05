package common

import (
	"math"
	"testing"
	"time"
)

func TestHawkesIntensityClustering(t *testing.T) {
	h := NewHawkesIntensity(10.0) // fast decay

	t0 := int64(1000000000)
	// Rapid burst of buy trades
	for i := 0; i < 5; i++ {
		h.Update(t0+int64(i*10000000), true, 1.5) // every 10ms
	}

	imb := h.Imbalance(t0 + 50000000)
	if imb <= 0.3 {
		t.Errorf("expected strong positive Hawkes buy imbalance after burst, got %v", imb)
	}

	// Long time passes, intensity should decay towards zero
	tLater := t0 + 5000000000 // 5 seconds later
	imbDecayed := h.Imbalance(tLater)
	if math.Abs(imbDecayed) > 0.15 {
		t.Errorf("expected decayed near-zero imbalance after 5s, got %v", imbDecayed)
	}
}

func TestVPINToxicity(t *testing.T) {
	vpin := NewVPINTracker(10.0, 5) // 10 vol per bucket, 5 buckets

	// Pure one-sided buying (severe toxicity / sweep)
	for i := 0; i < 5; i++ {
		vpin.AddTrade(true, 10.0)
	}

	tox := vpin.Toxicity()
	if tox < 0.95 {
		t.Errorf("expected near 1.0 toxicity for one-sided sweep, got %v", tox)
	}

	// Balanced trades (low toxicity)
	vpinBalanced := NewVPINTracker(10.0, 5)
	for i := 0; i < 5; i++ {
		vpinBalanced.AddTrade(true, 5.0)
		vpinBalanced.AddTrade(false, 5.0)
	}
	toxBalanced := vpinBalanced.Toxicity()
	if toxBalanced > 0.05 {
		t.Errorf("expected near 0.0 toxicity for balanced trades, got %v", toxBalanced)
	}
}

func TestStoikovMicropriceProperties(t *testing.T) {
	bestBid := int64(50000)
	bestAsk := int64(50010)
	mid := 50005.0

	// Equal quantities -> microprice = mid
	mpMid := StoikovMicroprice(bestBid, bestAsk, 100, 100)
	if math.Abs(mpMid-mid) > 0.01 {
		t.Errorf("expected microprice == mid for symmetric book, got %v", mpMid)
	}

	// Heavy bid quantity -> microprice strictly > mid
	mpHeavyBid := StoikovMicroprice(bestBid, bestAsk, 500, 50)
	if mpHeavyBid <= mid {
		t.Errorf("expected microprice > mid for heavy bid book, got %v", mpHeavyBid)
	}

	// Heavy ask quantity -> microprice strictly < mid
	mpHeavyAsk := StoikovMicroprice(bestBid, bestAsk, 50, 500)
	if mpHeavyAsk >= mid {
		t.Errorf("expected microprice < mid for heavy ask book, got %v", mpHeavyAsk)
	}
}

func TestTripleBarrierEvaluation(t *testing.T) {
	tb := TripleBarrierLabeler{
		PtBps:   10.0,
		SlBps:   5.0,
		Horizon: 30 * time.Second,
	}

	t0 := int64(1000000000)
	timestamps := []int64{t0 + 1e9, t0 + 2e9, t0 + 3e9}

	// Scenario 1: Hits Profit Target (+1)
	midsWin := []float64{100.0, 100.05, 100.12} // +12 bps
	resWin := tb.Evaluate(true, 100.0, midsWin, timestamps, t0)
	if resWin.Label != 1 || resWin.ReturnBps != 10.0 {
		t.Errorf("expected profit target hit (+1), got %+v", resWin)
	}

	// Scenario 2: Hits Stop Loss (-1)
	midsLoss := []float64{100.0, 99.96, 99.93} // -7 bps
	resLoss := tb.Evaluate(true, 100.0, midsLoss, timestamps, t0)
	if resLoss.Label != -1 || resLoss.ReturnBps != -5.0 {
		t.Errorf("expected stop loss hit (-1), got %+v", resLoss)
	}
}

func TestMetaLabelingFilter(t *testing.T) {
	base := NewRidge(1e-3)
	meta := NewMetaLabelingFilter(base, 10.0, 0.55)

	// Create samples where informative feature X0 drives direction and X1 drives trade magnitude / conviction
	samples := make([]Sample, 120)
	for i := 0; i < 120; i++ {
		x0 := float64(i%6) - 2.5      // direction signal
		x1 := float64((i*3)%10) * 0.5 // conviction signal (0.0 to 4.5)
		y := x0 * (x1 * 3.0)          // magnitude proportional to x1
		samples[i] = Sample{
			X: [FeatureCount]float64{x0, x1},
			Y: y,
		}
	}

	if err := meta.Fit(samples, []int{0, 1}); err != nil {
		t.Fatalf("MetaLabeling fit failed: %v", err)
	}

	// Strong conviction feature x1=4.0 with positive direction x0=2.0 -> clears hurdle
	var xStrong [FeatureCount]float64
	xStrong[0] = 2.0
	xStrong[1] = 4.0
	predStrong, probStrong, okStrong := meta.PredictWithMeta(&xStrong)
	if !okStrong || predStrong <= 0 || probStrong < 0.55 {
		t.Errorf("expected meta-labeler to pass high-conviction trade, got pred=%v prob=%v ok=%v", predStrong, probStrong, okStrong)
	}

	// Weak conviction feature x1=0.1 (low magnitude, eaten by fees) -> filtered out
	var xWeak [FeatureCount]float64
	xWeak[0] = 0.5
	xWeak[1] = 0.1
	predWeak, _, okWeak := meta.PredictWithMeta(&xWeak)
	if okWeak && predWeak != 0 {
		t.Errorf("expected meta-labeler to filter out noisy sub-fee trade, got pred=%v ok=%v", predWeak, okWeak)
	}
}

func TestGLFTMarketMakerAndQueueSimulation(t *testing.T) {
	mm := NewGLFTMarketMaker(1e-3, 0.05, 0.0)
	samples := make([]Sample, 100)
	for i := 0; i < 100; i++ {
		x0 := float64(i%5) - 2.0
		var x [FeatureCount]float64
		x[0] = x0
		x[3] = x0 * 0.1 // micro_dev
		x[4] = 1.0      // spread_bps
		x[5] = x0 * 1.5 // ofi_1s
		x[10] = 2.0     // vol_30s_bps
		x[14] = 8.0     // trade_cnt_1s
		x[15] = 0.5     // queue_ratio
		samples[i] = Sample{X: x, Y: x0 * 1.8}
	}

	if err := mm.Fit(samples, []int{0, 3, 4, 5, 10}); err != nil {
		t.Fatalf("GLFTMarketMaker.Fit failed: %v", err)
	}

	var xTest [FeatureCount]float64
	xTest[0] = 1.0
	xTest[3] = 0.2
	xTest[4] = 1.0
	xTest[10] = 2.5

	qFlat := mm.Quote(&xTest, 0.0)
	qLong := mm.Quote(&xTest, 3.0)
	qShort := mm.Quote(&xTest, -3.0)

	// Inventory skew invariant: holding long inventory must shift reservation price downward
	if qLong.ReservationBps >= qFlat.ReservationBps {
		t.Errorf("expected long inventory to skew reservation price downward: long=%v flat=%v", qLong.ReservationBps, qFlat.ReservationBps)
	}
	if qShort.ReservationBps <= qFlat.ReservationBps {
		t.Errorf("expected short inventory to skew reservation price upward: short=%v flat=%v", qShort.ReservationBps, qFlat.ReservationBps)
	}

	sim := SimulateQueueMakerFills(samples, mm)
	if sim.FillRate <= 0 || sim.FillRate > 1.0 {
		t.Errorf("expected valid queue fill rate in (0, 1], got %v", sim.FillRate)
	}
}

func TestRegimeSwitchingEnsemble(t *testing.T) {
	ens := NewRegimeSwitchingEnsemble(1e-3)
	samples := make([]Sample, 120)
	for i := 0; i < 120; i++ {
		x0 := float64(i%7) - 3.0
		var x [FeatureCount]float64
		x[0] = x0
		x[4] = 0.8
		x[8] = x0 * 1.2
		x[10] = 1.5
		samples[i] = Sample{X: x, Y: x0 * 2.0}
	}

	if err := ens.Fit(samples, []int{0, 4, 8, 10}); err != nil {
		t.Fatalf("RegimeSwitchingEnsemble.Fit failed: %v", err)
	}

	// Test Calm Regime classification
	var xCalm [FeatureCount]float64
	xCalm[4] = 0.8
	xCalm[10] = 1.2
	dCalm := ClassifyRegime(&xCalm, 1.5, 0.8)
	if dCalm.Regime != RegimeCalmMaker {
		t.Errorf("expected calm_maker regime, got %v", dCalm.Regime)
	}

	// Test Toxic Cascade Regime classification
	var xToxic [FeatureCount]float64
	xToxic[4] = 4.5   // wide spread
	xToxic[10] = 8.0  // volatility spike
	xToxic[11] = -8.5 // sharp 1s drop
	dToxic := ClassifyRegime(&xToxic, 1.5, 0.8)
	if dToxic.Regime != RegimeToxicCascade {
		t.Errorf("expected toxic_cascade regime, got %v", dToxic.Regime)
	}

	var xSignal [FeatureCount]float64
	xSignal[0] = 2.0
	xSignal[4] = 0.8
	xSignal[8] = 2.4
	xSignal[10] = 1.5
	pred := ens.Predict(&xSignal)
	if pred <= 0 {
		t.Errorf("expected positive prediction from regime ensemble on bullish flow, got %v", pred)
	}
}
