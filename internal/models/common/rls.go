package common

import (
	"math"
)

// AdaptiveRLS implements Exponentially Weighted Recursive Least Squares with forgetting factor.
// It adapts continuous online weights to track non-stationary microstructure regime shifts.
type AdaptiveRLS struct {
	ForgettingFactor float64 // Discount factor lambda in (0, 1], typically 0.98 - 0.999
	PriorPrecision   float64 // Prior weight regularizer delta (e.g. 100.0)
	Mean             [FeatureCount]float64
	Std              [FeatureCount]float64
	Weights          [FeatureCount]float64
	Bias             float64
	Fitted           bool
}

// NewAdaptiveRLS constructs an unfitted RLS estimator.
func NewAdaptiveRLS(forgettingFactor float64) *AdaptiveRLS {
	if forgettingFactor <= 0.80 || forgettingFactor > 1.0 {
		forgettingFactor = 0.99
	}
	return &AdaptiveRLS{
		ForgettingFactor: forgettingFactor,
		PriorPrecision:   100.0,
	}
}

// Fit processes sequential chronological samples with recursive matrix updates.
func (r *AdaptiveRLS) Fit(samples []Sample, idx []int) error {
	n := len(samples)
	if n == 0 || len(idx) == 0 {
		return ErrNotFitted
	}

	// 1. Compute standardization statistics over the window
	var meanX, stdX [FeatureCount]float64
	for _, j := range idx {
		var sum, sumSq float64
		for i := range samples {
			v := samples[i].X[j]
			sum += v
			sumSq += v * v
		}
		m := sum / float64(n)
		variance := sumSq/float64(n) - m*m
		s := 1.0
		if variance > 0 {
			s = math.Sqrt(variance)
		}
		if s < 1e-12 {
			s = 1.0
		}
		meanX[j] = m
		stdX[j] = s
	}
	r.Mean = meanX
	r.Std = stdX

	// Dimensions: bias + k features
	k := len(idx)
	if k > FeatureCount {
		k = FeatureCount
	}
	d := k + 1

	// Weights, intermediate vectors, and covariance matrix are stack-allocated for 0 allocations.
	var w, u, pu, gain [FeatureCount + 1]float64
	var p [FeatureCount + 1][FeatureCount + 1]float64
	for i := 0; i < d; i++ {
		p[i][i] = 1.0 / r.PriorPrecision
	}

	lam := r.ForgettingFactor
	invLam := 1.0 / lam

	for s := range samples {
		u[0] = 1.0
		for j, jj := range idx {
			u[j+1] = (samples[s].X[jj] - meanX[jj]) / stdX[jj]
		}
		y := samples[s].Y

		// Compute P * u
		for i := 0; i < d; i++ {
			var sum float64
			for j := 0; j < d; j++ {
				sum += p[i][j] * u[j]
			}
			pu[i] = sum
		}

		// Denominator: lambda + u^T * P * u
		var uPu float64
		for i := 0; i < d; i++ {
			uPu += u[i] * pu[i]
		}
		denom := lam + uPu
		if denom < 1e-12 {
			denom = 1e-12
		}
		invDenom := 1.0 / denom

		// Kalman gain: K = (P * u) / denom
		for i := 0; i < d; i++ {
			gain[i] = pu[i] * invDenom
		}

		// A priori error: e = y - u^T * w
		var pred float64
		for i := 0; i < d; i++ {
			pred += u[i] * w[i]
		}
		err := y - pred

		// Update weights: w = w + K * err
		for i := 0; i < d; i++ {
			w[i] += gain[i] * err
		}

		// Update covariance: P = (1 / lambda) * (P - K * (u^T * P))
		// Note that u^T * P = (P * u)^T = pu^T
		for i := 0; i < d; i++ {
			for j := 0; j < d; j++ {
				p[i][j] = (p[i][j] - gain[i]*pu[j]) * invLam
			}
		}

		// Ensure numerical symmetry
		for i := 0; i < d; i++ {
			for j := i + 1; j < d; j++ {
				avg := (p[i][j] + p[j][i]) * 0.5
				p[i][j] = avg
				p[j][i] = avg
			}
		}
	}

	// Map coefficients back to unstandardized feature space
	r.Bias = w[0]
	for j, jj := range idx {
		coeff := w[j+1] / stdX[jj]
		r.Weights[jj] = coeff
		r.Bias -= coeff * meanX[jj]
	}

	r.Fitted = true
	return nil
}

// Predict forecasts basis point return using adaptive weights.
func (r *AdaptiveRLS) Predict(x *[FeatureCount]float64) float64 {
	if !r.Fitted {
		return 0
	}
	v := r.Bias
	for j := 0; j < FeatureCount; j++ {
		v += r.Weights[j] * x[j]
	}
	return v
}

// Coefs returns the model weights.
func (r *AdaptiveRLS) Coefs(idx []int) map[int]float64 {
	out := make(map[int]float64, len(idx))
	for _, j := range idx {
		out[j] = r.Weights[j]
	}
	return out
}
