package experiment

import (
	"math"

	"piquant-live/internal/models/common"
)

// RunWalkForwardValidation evaluates expanding/rolling walk-forward out-of-sample
// Information Coefficient (IC_t) across chronological windows to measure temporal
// parameter decay and auto-retrain readiness.
//
// Returns:
//   - meanOOSIC: mean out-of-sample Pearson Information Coefficient across windows
//   - stabilityRatio: ICIR = mean(IC_t) / std(IC_t) (or bounded stability score when std is tiny)
//   - windowICs: slice of per-window out-of-sample ICs
func RunWalkForwardValidation(samples []common.Sample, windows int, buildAndFit func(train []common.Sample) func([]float64) float64) (meanOOSIC float64, stabilityRatio float64, windowICs []float64) {
	n := len(samples)
	if windows <= 0 {
		windows = 4
	}
	if n < (windows+1)*4 || buildAndFit == nil {
		return 0, 0, nil
	}

	step := n / (windows + 1)
	if step < 4 {
		return 0, 0, nil
	}

	windowICs = make([]float64, 0, windows)
	featBuf := make([]float64, common.FeatureCount)

	for w := 0; w < windows; w++ {
		trainEnd := (w + 1) * step
		oosStart := trainEnd
		oosEnd := oosStart + step
		if w == windows-1 || oosEnd > n {
			oosEnd = n
		}
		if oosEnd-oosStart < 3 {
			break
		}

		trainSlice := samples[:trainEnd]
		oosSlice := samples[oosStart:oosEnd]

		predictFn := buildAndFit(trainSlice)
		if predictFn == nil {
			continue
		}

		m := len(oosSlice)
		preds := make([]float64, m)
		actuals := make([]float64, m)
		var sumP, sumA float64
		for i := 0; i < m; i++ {
			copy(featBuf, oosSlice[i].X[:])
			p := predictFn(featBuf)
			a := oosSlice[i].Y
			preds[i] = p
			actuals[i] = a
			sumP += p
			sumA += a
		}

		meanP := sumP / float64(m)
		meanA := sumA / float64(m)
		var cov, varP, varA float64
		for i := 0; i < m; i++ {
			dp := preds[i] - meanP
			da := actuals[i] - meanA
			cov += dp * da
			varP += dp * dp
			varA += da * da
		}
		denom := math.Sqrt(varP * varA)
		var ic float64
		if denom > 1e-12 {
			ic = cov / denom
		}
		windowICs = append(windowICs, ic)
	}

	k := len(windowICs)
	if k == 0 {
		return 0, 0, windowICs
	}

	var sumIC float64
	for _, ic := range windowICs {
		sumIC += ic
	}
	meanOOSIC = sumIC / float64(k)

	var sumSqDiff float64
	for _, ic := range windowICs {
		d := ic - meanOOSIC
		sumSqDiff += d * d
	}
	stdIC := math.Sqrt(sumSqDiff / float64(k))
	if stdIC < 1e-4 {
		if math.Abs(meanOOSIC) > 1e-6 {
			stabilityRatio = math.Copysign(10.0, meanOOSIC)
		} else {
			stabilityRatio = 0
		}
	} else {
		stabilityRatio = meanOOSIC / stdIC
	}

	return meanOOSIC, stabilityRatio, windowICs
}

// ShouldThrottleForThermal implements the Raspberry Pi 5 thermal guardrail:
//   - tempC <= 75.0°C: nominal (no throttling, 4 parallel workers on Cortex-A76)
//   - 75.0°C < tempC <= 80.0°C: warm step-down (throttle=true, 2 workers)
//   - tempC > 80.0°C: critical thermal limit (throttle=true, 1 worker / offload to Pi 3B)
func ShouldThrottleForThermal(tempC float64) (throttle bool, recommendedWorkers int) {
	if math.IsNaN(tempC) || tempC <= 75.0 {
		return false, 4
	}
	if tempC <= 80.0 {
		return true, 2
	}
	return true, 1
}
