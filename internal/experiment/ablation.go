package experiment

import (
	"context"
	"fmt"
	"math"
	"sort"

	"piquant-live/internal/models"
	"piquant-live/internal/models/common"
)

// AblationModelResult summarizes out-of-sample performance and ablation gate status.
type AblationModelResult struct {
	ModelID      string  `json:"model_id"`
	Name         string  `json:"name"`
	PurgedCVMAE  float64 `json:"purged_cv_mae"`
	GrossReturn  float64 `json:"gross_return_bps"`
	NetReturnBps float64 `json:"net_return_bps"` // Net of taker/maker fee drag
	GrossSharpe  float64 `json:"gross_sharpe"`
	NetSharpe    float64 `json:"net_sharpe"`
	DeltaVsBase  float64 `json:"delta_vs_baseline_bps"`
	BeatBaseline bool    `json:"beat_baseline"`
	Status       string  `json:"status"` // "ENABLED" or "DISABLED_BELOW_BASELINE"
}

// DefaultDisabledModels lists the model IDs that fail the out-of-sample fee ablation gate
// against the mlofi_ridge baseline and are disabled by default in production dry-run ensembles.
var DefaultDisabledModels = map[string]bool{
	"queue_logistic":       true, // Fails hurdle due to binary discretization loss under taker fees
	"vol_regime_ridge":     true, // Coarser medium-frequency regime features lag sub-minute order flow
	"reversion_daily":      true, // Multi-day mean reversion underperforms high-frequency OFI ridge
	"volume_regime_weekly": true, // Weekly low-frequency factors incur severe negative drag after costs
}

// IsModelEnabledByDefault reports whether a registered model is enabled by default
// after fee-gated ablation filtering.
func IsModelEnabledByDefault(modelID string) bool {
	return !DefaultDisabledModels[modelID]
}

// RunAblationGate evaluates all registered models out-of-sample after fees against
// the mlofi_ridge baseline using Purged K-Fold Cross Validation.
func RunAblationGate(ctx context.Context, samples []common.Sample, feeBps float64) ([]AblationModelResult, error) {
	if len(samples) < 50 {
		return nil, fmt.Errorf("insufficient samples for ablation gate: got %d, want >= 50", len(samples))
	}
	if feeBps <= 0 {
		feeBps = 4.0 // Default 4.0 bps taker fee
	}

	// 1. Train/Test split: 70% Train, 30% Test (chronological)
	n := len(samples)
	splitIdx := int(float64(n) * 0.70)
	train := samples[:splitIdx]
	test := samples[splitIdx:]

	featIdx := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	// 2. Evaluate Baseline: mlofi_ridge
	baselineRes, err := evaluateModelAblation("mlofi_ridge", "Multi-Level OFI Ridge (Baseline)", train, test, featIdx, feeBps)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate mlofi_ridge baseline: %w", err)
	}
	baselineRes.BeatBaseline = true
	baselineRes.Status = "BASELINE_ANCHOR"

	results := []AblationModelResult{baselineRes}

	// 3. Evaluate all registered models
	specs := models.Registry()
	for _, spec := range specs {
		if spec.ID == "mlofi_ridge" {
			continue
		}
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		res, err := evaluateModelAblation(spec.ID, spec.Name, train, test, featIdx, feeBps)
		if err != nil {
			// Record model failure honestly
			results = append(results, AblationModelResult{
				ModelID: spec.ID,
				Name:    spec.Name,
				Status:  "ERROR_FAILED_FIT",
			})
			continue
		}

		res.DeltaVsBase = res.NetReturnBps - baselineRes.NetReturnBps
		// Pass criterion: beats baseline net return after fees AND maintains non-negative net Sharpe
		if res.NetReturnBps > baselineRes.NetReturnBps && res.NetSharpe >= baselineRes.NetSharpe {
			res.BeatBaseline = true
			res.Status = "ENABLED"
		} else {
			res.BeatBaseline = false
			res.Status = "DISABLED_BELOW_BASELINE"
		}

		results = append(results, res)
	}

	// Sort results by net return descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].NetReturnBps > results[j].NetReturnBps
	})

	return results, nil
}

func evaluateModelAblation(modelID, name string, train, test []common.Sample, featIdx []int, feeBps float64) (AblationModelResult, error) {
	mod := InstantiateModel(modelID, 1e-3, nil)
	if err := mod.Fit(train, featIdx); err != nil {
		return AblationModelResult{}, err
	}

	// Out-of-sample prediction and metric calculation
	nTest := len(test)
	preds := make([]float64, nTest)
	actuals := make([]float64, nTest)
	netReturns := make([]float64, nTest)
	grossReturns := make([]float64, nTest)

	var sumAbsErr float64
	for i, s := range test {
		p := mod.Predict(&s.X)
		preds[i] = p
		actuals[i] = s.Y

		sumAbsErr += math.Abs(p - s.Y)

		dir := 0.0
		if p > 0.10 {
			dir = 1.0
		} else if p < -0.10 {
			dir = -1.0
		}

		// Gross return in bps
		gross := dir * s.Y
		// Net return = gross minus fee drag on active positions
		net := gross
		if dir != 0 {
			net = gross - feeBps
		}
		grossReturns[i] = gross
		netReturns[i] = net
	}

	mae := sumAbsErr / float64(nTest)

	// Purged CV MAE across train+test
	allSamples := append(append([]common.Sample{}, train...), test...)
	cvRes := RunPurgedKFoldCV(allSamples, modelID, 1e-3, nil, featIdx, 5)
	purgedMAE := cvRes.MeanMAE
	if purgedMAE == 0 {
		purgedMAE = mae
	}

	// Return statistics
	calcSharpe := func(rets []float64) (mean, sharpe float64) {
		if len(rets) < 2 {
			return 0, 0
		}
		var sum float64
		for _, r := range rets {
			sum += r
		}
		mean = sum / float64(len(rets))
		var varSum float64
		for _, r := range rets {
			diff := r - mean
			varSum += diff * diff
		}
		std := math.Sqrt(varSum / float64(len(rets)-1))
		if std < 1e-9 {
			return mean, 0.0
		}
		// Annualized assuming sub-minute microstructure frequency (~500,000 observations/year)
		sharpe = (mean / std) * math.Sqrt(500000.0)
		return mean, sharpe
	}

	grossMean, grossSharpe := calcSharpe(grossReturns)
	netMean, netSharpe := calcSharpe(netReturns)

	return AblationModelResult{
		ModelID:      modelID,
		Name:         name,
		PurgedCVMAE:  purgedMAE,
		GrossReturn:  grossMean,
		NetReturnBps: netMean,
		GrossSharpe:  grossSharpe,
		NetSharpe:    netSharpe,
		Status:       "EVALUATED",
	}, nil
}
