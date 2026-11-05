package common

import (
	"math"
	"sort"
)

// SplitConformalCalibrator computes finite-sample, distribution-free non-conformity
// quantiles \hat{q}_{1-\alpha} (e.g., \alpha = 0.10 for 90% empirical coverage,
// conditioned on local volatility regime) from out-of-sample calibration residuals:
//
//	R_i = |y_i - \hat{y}_i| / \max(\sigma_i, \epsilon)
//
// Both Interval and PassesConformalGate execute in O(1) time with 0 heap allocations.
type SplitConformalCalibrator struct {
	Alpha          float64    // Miscoverage rate \alpha \in (0, 1), default 0.10 (90% coverage)
	Epsilon        float64    // Minimum volatility floor \epsilon > 0
	Quantile       float64    // Global finite-sample conformal quantile \hat{q}_{1-\alpha}
	RegimeQuantile [3]float64 // Regime-conditioned quantiles: [0]=Calm, [1]=Normal, [2]=HighVol
	VolLowThresh   float64    // Threshold separating Calm vs Normal volatility
	VolHighThresh  float64    // Threshold separating Normal vs HighVol volatility
	Calibrated     bool
}

// NewSplitConformalCalibrator constructs a SplitConformalCalibrator for miscoverage rate alpha.
func NewSplitConformalCalibrator(alpha float64) *SplitConformalCalibrator {
	if alpha <= 0 || alpha >= 1.0 {
		alpha = 0.10
	}
	return &SplitConformalCalibrator{
		Alpha:          alpha,
		Epsilon:        0.20,
		Quantile:       1.645,
		RegimeQuantile: [3]float64{1.40, 1.645, 1.96},
		VolLowThresh:   1.0,
		VolHighThresh:  2.5,
	}
}

// Calibrate fits the finite-sample non-conformity quantile \hat{q}_{1-\alpha} =
// Quantile(\lceil (n+1)(1-\alpha) \rceil / n) from calibration predictions and samples.
func (c *SplitConformalCalibrator) Calibrate(samples []Sample, preds []float64) {
	n := len(samples)
	if n > len(preds) {
		n = len(preds)
	}
	if n == 0 {
		return
	}
	if c.Alpha <= 0 || c.Alpha >= 1.0 {
		c.Alpha = 0.10
	}
	if c.Epsilon <= 0 {
		c.Epsilon = 0.20
	}

	residuals := make([]float64, n)
	vols := make([]float64, n)
	for i := 0; i < n; i++ {
		vol := samples[i].Vol
		if vol <= 0 {
			vol = math.Abs(samples[i].X[10])
		}
		denom := math.Max(vol, c.Epsilon)
		vols[i] = denom
		residuals[i] = math.Abs(samples[i].Y-preds[i]) / denom
	}

	sortedVols := make([]float64, n)
	copy(sortedVols, vols)
	sort.Float64s(sortedVols)
	c.VolLowThresh = sortedVols[n/3]
	c.VolHighThresh = sortedVols[(2*n)/3]
	if c.VolHighThresh <= c.VolLowThresh {
		c.VolHighThresh = c.VolLowThresh + 0.5
	}

	var rCalm, rNorm, rHigh []float64
	for i := 0; i < n; i++ {
		v := vols[i]
		if v <= c.VolLowThresh {
			rCalm = append(rCalm, residuals[i])
		} else if v <= c.VolHighThresh {
			rNorm = append(rNorm, residuals[i])
		} else {
			rHigh = append(rHigh, residuals[i])
		}
	}

	sort.Float64s(residuals)
	c.Quantile = conformalFiniteSampleQuantile(residuals, c.Alpha)
	c.RegimeQuantile[0] = conformalFiniteSampleQuantileOrDefault(rCalm, c.Alpha, c.Quantile*0.90)
	c.RegimeQuantile[1] = conformalFiniteSampleQuantileOrDefault(rNorm, c.Alpha, c.Quantile)
	c.RegimeQuantile[2] = conformalFiniteSampleQuantileOrDefault(rHigh, c.Alpha, c.Quantile*1.12)
	c.Calibrated = true
}

// CalibrateResiduals calibrates directly from pre-computed actuals, predictions, and local volatilities.
func (c *SplitConformalCalibrator) CalibrateResiduals(actual, preds, localVols []float64) {
	n := len(actual)
	if len(preds) < n {
		n = len(preds)
	}
	if n == 0 {
		return
	}
	samples := make([]Sample, n)
	for i := 0; i < n; i++ {
		v := 1.0
		if i < len(localVols) && localVols[i] > 0 {
			v = localVols[i]
		}
		samples[i] = Sample{Y: actual[i], Vol: v}
	}
	c.Calibrate(samples, preds)
}

func conformalFiniteSampleQuantile(sorted []float64, alpha float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 1.645
	}
	// Finite-sample split conformal index: k = ceil((n + 1) * (1 - alpha)) - 1
	k := int(math.Ceil(float64(n+1)*(1.0-alpha))) - 1
	if k < 0 {
		k = 0
	}
	if k >= n {
		k = n - 1
	}
	q := sorted[k]
	if q < 0.05 {
		q = 0.05
	}
	return q
}

func conformalFiniteSampleQuantileOrDefault(vals []float64, alpha, fallback float64) float64 {
	if len(vals) < 4 {
		return fallback
	}
	sort.Float64s(vals)
	return conformalFiniteSampleQuantile(vals, alpha)
}

// Interval returns the distribution-free prediction interval [lower, upper] and a normalized
// certitude score in [0, 1] for a point forecast and local volatility in 0 allocs/op.
func (c *SplitConformalCalibrator) Interval(pred, localVol float64) (lower, upper, certitude float64) {
	eps := c.Epsilon
	if eps <= 0 {
		eps = 0.20
	}
	sigma := localVol
	if sigma < eps {
		sigma = eps
	}

	q := c.Quantile
	if q <= 0 {
		q = 1.645
	}
	if c.Calibrated {
		if sigma <= c.VolLowThresh {
			q = c.RegimeQuantile[0]
		} else if sigma <= c.VolHighThresh {
			q = c.RegimeQuantile[1]
		} else {
			q = c.RegimeQuantile[2]
		}
	}

	radius := q * sigma
	lower = pred - radius
	upper = pred + radius

	// Signal-to-uncertainty ratio mapped to [0, 1] certitude
	snr := math.Abs(pred) / math.Max(radius, 1e-6)
	certitude = 1.0 - math.Exp(-0.85*snr)
	if certitude < 0 {
		certitude = 0
	} else if certitude > 1.0 {
		certitude = 1.0
	}
	return lower, upper, certitude
}

// PassesConformalGate returns true when the conservative conformal bound (or signal-to-conformal
// uncertainty edge) clears the transaction cost hurdle in 0 allocs/op.
func (c *SplitConformalCalibrator) PassesConformalGate(pred, localVol, costHurdle float64) bool {
	if costHurdle < 0 {
		costHurdle = 0
	}
	lower, upper, certitude := c.Interval(pred, localVol)
	if pred > 0 {
		// Strict lower bound > costHurdle OR high-certitude directional edge clearing hurdle
		return lower > costHurdle || (pred > costHurdle && certitude >= 0.52)
	}
	if pred < 0 {
		return upper < -costHurdle || (-pred > costHurdle && certitude >= 0.52)
	}
	return false
}
