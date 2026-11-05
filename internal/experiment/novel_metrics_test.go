package experiment

import (
	"context"
	"math"
	"testing"

	"piquant-live/internal/models/common"
)

func TestDeflatedSharpeRatio(t *testing.T) {
	// Strong Sharpe (3.5) with 250 samples and 5 trials should have high DSR (> 0.70)
	dsrStrong := ComputeDeflatedSharpe(3.5, 250, 0.0, 3.0, 5)
	if dsrStrong < 0.70 {
		t.Errorf("expected high Deflated Sharpe for SR=3.5, got %v", dsrStrong)
	}

	// Weak Sharpe (0.4) over 50 trials should be heavily penalized by multiple-testing hurdle (< 0.35)
	dsrWeak := ComputeDeflatedSharpe(0.4, 100, -0.5, 5.0, 50)
	if dsrWeak >= dsrStrong || dsrWeak > 0.45 {
		t.Errorf("expected deflated sharpe penalty for weak SR under 50 trials, got %v", dsrWeak)
	}
}

func TestPurgedKFoldCV(t *testing.T) {
	samples := make([]common.Sample, 150)
	for i := 0; i < 150; i++ {
		x0 := math.Sin(float64(i) * 0.15)
		var x [common.FeatureCount]float64
		x[0] = x0
		x[1] = x0 * 0.8
		samples[i] = common.Sample{
			X: x,
			Y: x0 * 2.5,
		}
	}

	res := RunPurgedKFoldCV(samples, "mlofi_ridge", 1e-3, nil, []int{0, 1}, 5)
	if res.Folds != 5 {
		t.Fatalf("expected 5 valid purged folds, got %d", res.Folds)
	}
	if res.MeanMAE <= 0 || res.MeanMAE > 1.0 {
		t.Errorf("expected low out-of-fold MeanMAE on clean linear signal, got %v", res.MeanMAE)
	}
	if res.ProbBacktestOverfit > 0.25 {
		t.Errorf("expected low PBO on true linear signal, got %v", res.ProbBacktestOverfit)
	}
}

func TestCrossAssetKalmanLeadLag(t *testing.T) {
	leader := make([]FeatureObservation, 60)
	follower := make([]FeatureObservation, 60)

	for i := 0; i < 60; i++ {
		btcMove := float64(i)*0.5 + math.Sin(float64(i)*0.3)*2.0
		ethMove := 1.2*btcMove + math.Cos(float64(i)*0.5)*0.5

		var fLeader, fFollower [common.FeatureCount]float64
		fLeader[8] = math.Cos(float64(i) * 0.3) // leader MLOFI_1s
		if i > 0 {
			fFollower[11] = math.Cos(float64(i-1)*0.3) * 1.5 // follower 1s return lags leader MLOFI
		}

		leader[i] = FeatureObservation{
			Timestamp: int64(i) * 1e9,
			MidPrice:  50000.0 * (1.0 + btcMove*1e-4),
			Features:  fLeader,
		}
		follower[i] = FeatureObservation{
			Timestamp: int64(i) * 1e9,
			MidPrice:  3000.0 * (1.0 + ethMove*1e-4),
			Features:  fFollower,
		}
	}

	stats := EvaluateCrossAssetLeadLag(leader, follower)
	if stats.KalmanBeta < 0.7 || stats.KalmanBeta > 1.7 {
		t.Errorf("expected Kalman hedge beta near 1.2, got %v", stats.KalmanBeta)
	}
	if stats.CrossFlowCorr <= 0.3 {
		t.Errorf("expected positive cross-asset flow correlation, got %v", stats.CrossFlowCorr)
	}
}

func TestWorkerEvalOffloadFallback(t *testing.T) {
	train := make([]common.Sample, 40)
	val := make([]common.Sample, 20)
	for i := 0; i < 40; i++ {
		x0 := float64(i%5) - 2.0
		train[i] = common.Sample{X: [common.FeatureCount]float64{x0}, Y: x0 * 1.5}
	}
	for i := 0; i < 20; i++ {
		x0 := float64(i%5) - 2.0
		val[i] = common.Sample{X: [common.FeatureCount]float64{x0}, Y: x0 * 1.5}
	}

	req := WorkerEvalRequest{
		TrialID:      1,
		Model:        "glft_maker",
		FeatureSet:   "L1",
		Lambda:       1e-3,
		TrainSamples: train,
		ValSamples:   val,
	}

	// Test direct worker evaluation
	resp := EvaluateWorkerTrial(req, "pi3b")
	if !resp.OK || resp.WorkerNode != "pi3b" || resp.Trial.ValMAE <= 0 {
		t.Errorf("expected valid worker evaluation, got %+v", resp)
	}

	// Test transparent fallback when peer address is unreachable
	fbResp := OffloadTrialToPeer(context.Background(), "", "", req)
	if !fbResp.OK || fbResp.WorkerNode != "pi5" {
		t.Errorf("expected transparent fallback to pi5 when peer is empty, got %+v", fbResp)
	}
}
