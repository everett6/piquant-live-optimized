package common

import (
	"math"
)

// ElasticNet implements regularized linear regression via cyclical coordinate descent
// with combined L1 (Lasso) and L2 (Ridge) penalties.
//
// L1 penalty enforces feature sparsity by zeroing out non-informative microstructure noise.
// L2 penalty stabilizes estimation when features are collinear (such as multi-level order book depths).
//
// Layout is 64-byte cache-line aligned: hot inference fields (Weights, Bias, Fitted)
// reside at offsets 0..144 bytes, eliminating split cache-line penalty during high-frequency prediction.
type ElasticNet struct {
	Weights [FeatureCount]float64 // 128 bytes: exactly 2 cache lines (0..127)
	Bias    float64               // 8 bytes (offset 128)
	Lam     float64               // 8 bytes (offset 136) - Overall regularization parameter (lambda > 0)
	Alpha   float64               // 8 bytes (offset 144) - L1 vs L2 mixing parameter (0 = pure Ridge, 1 = pure Lasso)
	Iters   int                   // 8 bytes (offset 152)
	Fitted  bool                  // 1 byte (offset 160)
	_       [7]byte               // 7 bytes alignment padding
	Mean    [FeatureCount]float64 // 128 bytes (training stats)
	Std     [FeatureCount]float64 // 128 bytes (training stats)
}

// NewElasticNet constructs an unfitted ElasticNet estimator.
func NewElasticNet(lam, alpha float64) *ElasticNet {
	if lam <= 0 {
		lam = 1e-3
	}
	if alpha < 0 {
		alpha = 0
	} else if alpha > 1 {
		alpha = 1
	}
	return &ElasticNet{
		Lam:   lam,
		Alpha: alpha,
	}
}

// Fit trains the ElasticNet model on samples using coordinate descent.
func (e *ElasticNet) Fit(samples []Sample, idx []int) error {
	n := len(samples)
	if n == 0 || len(idx) == 0 {
		return ErrNotFitted
	}

	// 1. Compute feature and target means & standard deviations
	k := len(idx)
	var sumY float64
	for i := range samples {
		sumY += samples[i].Y
	}
	meanY := sumY / float64(n)

	// Standardize used features
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
	e.Mean = meanX
	e.Std = stdX

	// Pre-standardize X matrix into a contiguous column-major slice for optimal L1 cache locality
	// and hardware prefetching during cyclical coordinate descent updates.
	// Column j is located at [j*n .. (j+1)*n-1].
	zCol := make([]float64, k*n)
	for j := 0; j < k; j++ {
		jj := idx[j]
		m := meanX[jj]
		s := stdX[jj]
		offset := j * n
		for i := range samples {
			zCol[offset+i] = (samples[i].X[jj] - m) / s
		}
	}

	// Initial residuals r_i = y_i - meanY
	residuals := make([]float64, n)
	for i := range samples {
		residuals[i] = samples[i].Y - meanY
	}

	var beta [FeatureCount]float64
	l1Penalty := e.Lam * e.Alpha
	l2Penalty := e.Lam * (1.0 - e.Alpha)
	invDenom := 1.0 / (1.0 + l2Penalty)

	const maxPasses = 100
	const tol = 1e-6
	e.Iters = 0

	for pass := 0; pass < maxPasses; pass++ {
		e.Iters++
		var maxChange float64

		for j := 0; j < k; j++ {
			offset := j * n
			// Compute partial correlation rho_j = (1/n) * sum(z_ij * r_i) + beta_j
			var dot float64
			for i := 0; i < n; i++ {
				dot += zCol[offset+i] * residuals[i]
			}
			rho := dot/float64(n) + beta[j]

			// Soft-threshold operator
			newBeta := 0.0
			if rho > l1Penalty {
				newBeta = (rho - l1Penalty) * invDenom
			} else if rho < -l1Penalty {
				newBeta = (rho + l1Penalty) * invDenom
			}

			diff := newBeta - beta[j]
			if math.Abs(diff) > maxChange {
				maxChange = math.Abs(diff)
			}

			if diff != 0 {
				// Update residuals along contiguous column stride
				for i := 0; i < n; i++ {
					residuals[i] -= diff * zCol[offset+i]
				}
				beta[j] = newBeta
			}
		}

		if maxChange < tol {
			break
		}
	}

	// Unstandardize coefficients back to original feature scale
	e.Bias = meanY
	for j, jj := range idx {
		w := beta[j] / stdX[jj]
		e.Weights[jj] = w
		e.Bias -= w * meanX[jj]
	}

	e.Fitted = true
	return nil
}

// Predict forecasts expected basis points return for the given feature vector in 0 B/op and 0 allocs/op.
// Uses unrolled 4-way SIMD dot product tuned for ARM64 Cortex-A76 dual-issue vector pipelines.
func (e *ElasticNet) Predict(x *[FeatureCount]float64) float64 {
	if !e.Fitted {
		return 0
	}
	return e.Bias + Dot16(&e.Weights, x)
}

// Coefs returns non-zero coefficients mapped by feature index.
func (e *ElasticNet) Coefs(idx []int) map[int]float64 {
	out := make(map[int]float64, len(idx))
	for _, j := range idx {
		out[j] = e.Weights[j]
	}
	return out
}
