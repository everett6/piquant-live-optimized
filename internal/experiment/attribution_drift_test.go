package experiment

import (
	"math"
	"testing"

	"piquant-live/internal/models/common"
)

func TestComputeMarkoutAttributionAndEvaluatePredictions(t *testing.T) {
	n := 140
	samples := make([]common.Sample, n)
	preds := make([]float64, n)

	for i := 0; i < n; i++ {
		var x [common.FeatureCount]float64
		sig := math.Sin(float64(i)*0.18) * 3.2
		x[0] = math.Sin(float64(i) * 0.18)
		x[3] = 0.25 * x[0]
		x[4] = 1.0  // spread_bps
		x[5] = 1.6  // ofi_1s
		x[8] = 1.2  // mlofi_1s
		x[10] = 0.8 // vol_30s_bps
		x[13] = 0.5 // trade_imb_5s
		x[14] = 8.0 // trade_cnt_1s
		x[15] = 0.4 // queue_ratio

		samples[i] = common.Sample{
			X:      x,
			Y:      sig,
			Mid:    50000.0 + float64(i)*0.2,
			Spread: 1.0,
			Vol:    0.8,
		}
		preds[i] = sig * 0.92
	}

	spCap, dirAlpha, toxLoss, confPass := ComputeMarkoutAttribution(samples, preds, 0.05, 0.20)
	if spCap <= 0 {
		t.Errorf("expected positive SpreadCaptureBps, got %v", spCap)
	}
	if dirAlpha <= 0 {
		t.Errorf("expected positive DirectionalAlphaBps on aligned predictions, got %v", dirAlpha)
	}
	if toxLoss <= 0 {
		t.Errorf("expected positive MarkoutToxicityBps, got %v", toxLoss)
	}
	if confPass <= 0.20 || confPass > 1.0 {
		t.Errorf("expected ConformalPassRate in (0.20, 1.0], got %v", confPass)
	}

	m := EvaluatePredictions(samples, preds)
	if m.SpreadCaptureBps != spCap || m.DirectionalAlphaBps != dirAlpha || m.MarkoutToxicityBps != toxLoss || m.ConformalPassRate != confPass {
		t.Errorf("EvaluatePredictions did not populate markout attribution metrics: %+v", m)
	}
}

func TestPageHinkleyDriftGuardDetectionAndRollback(t *testing.T) {
	guard := NewPageHinkleyDriftGuard(0.15, 8.0)

	// 1. Feed 40 stationary low-residual observations
	for i := 0; i < 40; i++ {
		res := 0.30 + 0.05*math.Sin(float64(i)*0.3)
		featNorm := 2.0 + 0.1*math.Cos(float64(i)*0.3)
		detected, stat, thresh := guard.Update(res, featNorm)
		if detected {
			t.Fatalf("unexpected false-alarm drift during stationary warmup at step %d (stat=%v, thresh=%v)", i, stat, thresh)
		}
	}

	// 2. Inject abrupt structural break / residual & feature shift
	var triggered bool
	var peakStat float64
	for i := 0; i < 25; i++ {
		detected, stat, _ := guard.Update(6.5, 9.0)
		if stat > peakStat {
			peakStat = stat
		}
		if detected {
			triggered = true
			break
		}
	}

	if !triggered {
		t.Errorf("expected PageHinkleyDriftGuard to detect structural drift (peakStat=%v)", peakStat)
	}
	st := guard.Status()
	if st.RollbackCount < 1 {
		t.Errorf("expected RollbackCount >= 1 after drift detection, got %d", st.RollbackCount)
	}
}

func TestInstantiateRoughSignatureOFIModelInRunner(t *testing.T) {
	mod := InstantiateModel("rough_signature_ofi", 1e-3, nil)
	if mod == nil {
		t.Fatal("InstantiateModel returned nil for rough_signature_ofi")
	}
	samples := make([]common.Sample, 60)
	for i := 0; i < 60; i++ {
		var x [common.FeatureCount]float64
		x[0] = math.Sin(float64(i) * 0.2)
		x[5] = 1.5 * x[0]
		x[8] = 0.9 * x[0]
		samples[i] = common.Sample{X: x, Y: 2.0 * x[0]}
	}
	if err := mod.Fit(samples, []int{0, 5, 8}); err != nil {
		t.Fatalf("Fit failed: %v", err)
	}
	pred := mod.Predict(&samples[1].X)
	if !common.Finite(pred) || pred == 0 {
		t.Errorf("expected finite non-zero prediction from instantiated rough_signature_ofi, got %v", pred)
	}
}

func BenchmarkPageHinkleyDriftGuardUpdate(b *testing.B) {
	guard := NewPageHinkleyDriftGuard(0.15, 12.0)
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		_, stat, thresh := guard.Update(0.45, 2.35)
		sink += stat + thresh
	}
	if math.IsNaN(sink) {
		b.Fatal("NaN")
	}
}
