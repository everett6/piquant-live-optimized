package experiment

import (
	"context"
	"math/rand"
	"testing"

	"piquant-live/internal/models/common"
)

func TestAblationGateEvaluation(t *testing.T) {
	// Generate realistic synthetic microstructure samples
	r := rand.New(rand.NewSource(42))
	nSamples := 400
	samples := make([]common.Sample, nSamples)

	for i := 0; i < nSamples; i++ {
		var x [common.FeatureCount]float64
		// Simulate features
		x[0] = (r.Float64() - 0.5) * 2.0  // imb_l1
		x[3] = (r.Float64() - 0.5) * 0.5  // micro_dev
		x[4] = 0.50 + r.Float64()*1.0     // spread_bps
		x[5] = (r.Float64() - 0.5) * 4.0  // ofi_1s
		x[8] = x[5] * 0.8                 // mlofi_1s
		x[10] = 1.0 + r.Float64()*1.5     // vol_30s_bps
		x[11] = (r.Float64() - 0.5) * 1.0 // ret_1s_bps
		x[14] = 5.0 + r.Float64()*20.0    // trade_cnt_1s
		x[15] = 0.5 + x[0]*0.25           // queue_ratio

		// Forward mid return (target Y) driven by OFI and micro_dev with noise
		y := 0.65*x[5] + 0.40*x[8] + 0.30*x[3] + (r.Float64()-0.5)*1.5

		samples[i] = common.Sample{
			X:      x,
			Y:      y,
			Mid:    60000.0 + float64(i)*2.0,
			Spread: x[4],
			Vol:    x[10],
		}
	}

	results, err := RunAblationGate(context.Background(), samples, 1.0)
	if err != nil {
		t.Fatalf("RunAblationGate failed: %v", err)
	}

	t.Logf("\n%-26s %-12s %-14s %-12s %-14s %-10s %s",
		"Model ID", "Purged MAE", "Gross Ret(bps)", "Gross Sharpe", "Net Ret(bps)", "Delta", "Ablation Gate")
	t.Logf("---------------------------------------------------------------------------------------------------------")
	for _, res := range results {
		t.Logf("%-26s %-12.4f %-14.2f %-12.2f %-14.2f %+9.2f  %s",
			res.ModelID, res.PurgedCVMAE, res.GrossReturn, res.GrossSharpe, res.NetReturnBps, res.DeltaVsBase, res.Status)
	}

	if len(results) < 5 {
		t.Errorf("expected at least 5 evaluated models, got %d", len(results))
	}
}
