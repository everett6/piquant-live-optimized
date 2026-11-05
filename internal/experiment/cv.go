package experiment

import (
	"math"
	"sync"

	"piquant-live/internal/models/common"
)

// PurgedCVResult holds out-of-fold statistics from Purged K-Fold Cross-Validation.
type PurgedCVResult struct {
	Folds               int       `json:"folds"`
	MeanMAE             float64   `json:"mean_mae"`
	StdMAE              float64   `json:"std_mae"`
	MeanSharpe          float64   `json:"mean_sharpe"`
	ProbBacktestOverfit float64   `json:"prob_backtest_overfit"` // PBO estimate in [0, 1]
	FoldMAEs            []float64 `json:"fold_maes"`
}

// ComputeDeflatedSharpe calculates Bailey & López de Prado's Deflated Sharpe Ratio (DSR) in [0, 1],
// adjusting for sample length T, return skewness, excess tail kurtosis, and multiple-testing trials N.
func ComputeDeflatedSharpe(sharpe float64, nSamples int, skewness, kurtosis float64, nTrials int) float64 {
	if nSamples < 10 {
		return 0.5
	}
	if nTrials < 1 {
		nTrials = 1
	}

	// De-annualize Sharpe to per-observation scale for the variance formula
	annFactor := math.Sqrt(math.Min(float64(nSamples), 252.0))
	srPerObs := sharpe / annFactor

	// Expected maximum Sharpe Ratio under null across N trials (False Strategy Theorem)
	var sr0 float64
	if nTrials > 1 {
		const eulerGamma = 0.5772156649015329
		z1 := invNormalCDF(1.0 - 1.0/float64(nTrials))
		z2 := invNormalCDF(1.0 - 1.0/(float64(nTrials)*math.E))
		// Variance of trial Sharpe ratios ~ 1 / T under null
		stdTrialSR := 1.0 / math.Sqrt(float64(nSamples))
		sr0 = stdTrialSR * ((1.0-eulerGamma)*z1 + eulerGamma*z2)
	}

	// Non-Normal Sharpe Ratio estimation variance:
	// Var(SR) = (1 - skew*SR + ((kurt - 1)/4)*SR^2) / (T - 1)
	if kurtosis < 1.5 {
		kurtosis = 3.0 // Gaussian baseline if unestimated
	}
	inner := 1.0 - skewness*srPerObs + ((kurtosis-1.0)/4.0)*srPerObs*srPerObs
	if inner < 0.05 {
		inner = 0.05
	}
	srStdErr := math.Sqrt(inner / float64(nSamples-1))
	if srStdErr < 1e-9 {
		return 0.5
	}

	zStat := (srPerObs - sr0) / srStdErr
	return normalCDF(zStat)
}

// normalCDF computes the standard normal cumulative distribution function Phi(z).
func normalCDF(z float64) float64 {
	return 0.5 * (1.0 + math.Erf(z/math.Sqrt2))
}

// invNormalCDF approximates the inverse standard normal CDF Phi^{-1}(p) (Beasley-Springer-Moro).
func invNormalCDF(p float64) float64 {
	if p <= 0.0001 {
		return -3.719
	}
	if p >= 0.9999 {
		return 3.719
	}
	// Rational approximation for central and tail regions
	t := math.Sqrt(-2.0 * math.Log(1.0-p))
	c0, c1, c2 := 2.515517, 0.802853, 0.010328
	d1, d2, d3 := 1.432788, 0.189269, 0.001308
	return t - (c0+c1*t+c2*t*t)/(1.0+d1*t+d2*t*t+d3*t*t*t)
}

// RunPurgedKFoldCV executes K-Fold Cross-Validation with purging and embargo buffers
// around each validation fold to eliminate serial lookahead leakage across overlapping labels.
// Folds are evaluated in parallel across CPU cores using a bounded worker pool.
func RunPurgedKFoldCV(samples []common.Sample, modelName string, lambda float64, params map[string]any, featIdx []int, kFolds int) PurgedCVResult {
	n := len(samples)
	if kFolds < 2 {
		kFolds = 5
	}
	if n < kFolds*8 {
		return PurgedCVResult{Folds: kFolds}
	}

	foldSize := n / kFolds
	embargo := int(math.Max(2.0, float64(n)*0.02)) // 2% purge/embargo buffer on each boundary

	type foldResult struct {
		mae     float64
		sharpe  float64
		overfit bool
		valid   bool
	}
	foldResults := make([]foldResult, kFolds)

	maxWorkers := 4
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for k := 0; k < kFolds; k++ {
		valStart := k * foldSize
		valEnd := valStart + foldSize
		if k == kFolds-1 {
			valEnd = n
		}
		if valEnd <= valStart {
			continue
		}

		kIdx := k
		wg.Add(1)
		go func(k int, vStart, vEnd int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Construct purged + embargoed training set:
			// Exclude [vStart - embargo, vEnd + embargo)
			purgeLeft := vStart - embargo
			if purgeLeft < 0 {
				purgeLeft = 0
			}
			embargoRight := vEnd + embargo
			if embargoRight > n {
				embargoRight = n
			}

			trainSlice := make([]common.Sample, 0, n-(embargoRight-purgeLeft))
			if purgeLeft > 0 {
				trainSlice = append(trainSlice, samples[:purgeLeft]...)
			}
			if embargoRight < n {
				trainSlice = append(trainSlice, samples[embargoRight:]...)
			}
			if len(trainSlice) < 10 {
				return
			}

			valSlice := samples[vStart:vEnd]

			mod := InstantiateModel(modelName, lambda, params)
			if err := mod.Fit(trainSlice, featIdx); err != nil {
				return
			}

			// Evaluate in-sample train Sharpe vs out-of-fold val Sharpe to measure PBO
			trainPreds := make([]float64, len(trainSlice))
			trainActuals := make([]float64, len(trainSlice))
			for i, s := range trainSlice {
				trainPreds[i] = mod.Predict(&s.X)
				trainActuals[i] = s.Y
			}
			trainM := ComputeMetrics(trainPreds, trainActuals)

			valPreds := make([]float64, len(valSlice))
			valActuals := make([]float64, len(valSlice))
			for i, s := range valSlice {
				valPreds[i] = mod.Predict(&s.X)
				valActuals[i] = s.Y
			}
			valM := ComputeMetrics(valPreds, valActuals)

			// Fold exhibits backtest overfitting if in-sample Sharpe > 0 but out-of-fold Sharpe <= 0
			// or out-of-fold MAE degrades > 15% over zero-forecast baseline
			overfit := false
			zeroBaselineMAE := valM.Baselines["zero_mae"]
			if (trainM.SharpeRatio > 0.1 && valM.SharpeRatio < 0) || (zeroBaselineMAE > 0 && valM.MAE > zeroBaselineMAE*1.15) {
				overfit = true
			}

			foldResults[k] = foldResult{
				mae:     valM.MAE,
				sharpe:  valM.SharpeRatio,
				overfit: overfit,
				valid:   true,
			}
		}(kIdx, valStart, valEnd)
	}
	wg.Wait()

	foldMAEs := make([]float64, 0, kFolds)
	var sumMAE, sumSharpe float64
	var overfitFolds int

	for k := 0; k < kFolds; k++ {
		if foldResults[k].valid {
			foldMAEs = append(foldMAEs, foldResults[k].mae)
			sumMAE += foldResults[k].mae
			sumSharpe += foldResults[k].sharpe
			if foldResults[k].overfit {
				overfitFolds++
			}
		}
	}

	validFolds := len(foldMAEs)
	if validFolds == 0 {
		return PurgedCVResult{Folds: kFolds}
	}

	meanMAE := sumMAE / float64(validFolds)
	meanSharpe := sumSharpe / float64(validFolds)

	var sumSqDiff float64
	for _, m := range foldMAEs {
		d := m - meanMAE
		sumSqDiff += d * d
	}
	stdMAE := math.Sqrt(sumSqDiff / float64(validFolds))
	pbo := float64(overfitFolds) / float64(validFolds)

	return PurgedCVResult{
		Folds:               validFolds,
		MeanMAE:             meanMAE,
		StdMAE:              stdMAE,
		MeanSharpe:          meanSharpe,
		ProbBacktestOverfit: pbo,
		FoldMAEs:            foldMAEs,
	}
}
