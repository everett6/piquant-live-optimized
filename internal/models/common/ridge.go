package common

import "math"

// Ridge is a standardised linear model fitted by ridge regression.
//
// Inputs are standardised with training-set statistics and the intercept is
// unpenalised, so the penalty is comparable across features with wildly
// different scales (an OFI in base units next to an imbalance in [-1, 1]).
// The normal equations are solved by Gaussian elimination with partial
// pivoting on a matrix of at most FeatureCount x FeatureCount; there is no
// iteration limit to reach and no convergence to fail.
//
// It predicts a continuous target (a mid-price change in bps). Logistic
// classification shares the same standardisation and solver with a different
// objective, see Logistic.
type Ridge struct {
	Lam     float64 // L2 penalty on standardised coefficients
	Mean    [FeatureCount]float64
	Std     [FeatureCount]float64
	Weights [FeatureCount]float64
	Bias    float64
	Fitted  bool
}

// NewRidge returns an unfitted model with a default penalty.
func NewRidge(lam float64) *Ridge {
	if lam <= 0 {
		lam = 1e-3
	}
	return &Ridge{Lam: lam}
}

// design rows are (x, y) pairs; features listed in idx are used.
type Sample struct {
	X         [FeatureCount]float64
	Y         float64
	TsNs      int64
	Mid       float64
	Spread    float64
	Vol       float64
	TargetCls int64
}

// Standardise computes the training mean and standard deviation per feature.
// A feature that never varies gets std 1 so its standardised value is 0 and
// the penalty keeps its coefficient at 0, rather than dividing by zero.
func (r *Ridge) Standardise(samples []Sample, idx []int) {
	for _, j := range idx {
		var sum, sumSq float64
		for i := range samples {
			v := samples[i].X[j]
			sum += v
			sumSq += v * v
		}
		n := float64(len(samples))
		mean := sum / n
		varr := sumSq/n - mean*mean
		std := 1.0
		if varr > 0 {
			std = math.Sqrt(varr)
		}
		if std < 1e-12 {
			std = 1
		}
		r.Mean[j] = mean
		r.Std[j] = std
	}
}

// Fit solves the ridge normal equations over idx.
func (r *Ridge) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	r.Standardise(samples, idx)
	// Solve only over the used features to keep the system small.
	k := len(idx)
	if k > FeatureCount {
		k = FeatureCount
	}
	// Fixed-size stack array eliminates heap allocations entirely.
	var a [FeatureCount + 1][FeatureCount + 1]float64
	n := float64(len(samples))
	var row [FeatureCount]float64
	// Row 0 carries the bias (unpenalised), rows 1..k the features.
	a[0][0] = n
	for s := range samples {
		for j := 0; j < k; j++ {
			jj := idx[j]
			row[j] = (samples[s].X[jj] - r.Mean[jj]) / r.Std[jj]
		}
		y := samples[s].Y
		a[0][FeatureCount] += y
		for j := 0; j < k; j++ {
			rj := row[j]
			a[j+1][0] += rj
			a[j+1][FeatureCount] += rj * y
			for l := j; l < k; l++ {
				a[j+1][l+1] += rj * row[l]
			}
		}
	}
	// Symmetrize the feature matrix
	for j := 0; j < k; j++ {
		for l := j + 1; l < k; l++ {
			a[l+1][j+1] = a[j+1][l+1]
		}
	}
	// Bias row: the sum of the features is generally non-zero once the data
	// is standardised only within the training window, so keep it symmetric.
	for j := 0; j < k; j++ {
		a[0][j+1] = a[j+1][0]
	}
	for j := 1; j <= k; j++ {
		a[j][j] += r.Lam
	}
	var coef [FeatureCount + 1]float64
	if !solveFixed(&a, k+1, FeatureCount, &coef) {
		return ErrNotFitted
	}
	r.Bias = coef[0]
	for j := 0; j < k; j++ {
		jj := idx[j]
		r.Weights[jj] = coef[j+1] / r.Std[jj]
	}
	// Fold the standardisation into the intercept so Predict needs no mean.
	for j := 0; j < k; j++ {
		jj := idx[j]
		r.Bias -= r.Weights[jj] * r.Mean[jj]
	}
	r.Fitted = true
	return nil
}

// solveFixed performs Gauss-Jordan elimination on a stack-allocated matrix in place with zero heap allocations.
func solveFixed(a *[FeatureCount + 1][FeatureCount + 1]float64, n, rhsCol int, out *[FeatureCount + 1]float64) bool {
	for col := 0; col < n; col++ {
		pivot := col
		best := math.Abs(a[col][col])
		for row := col + 1; row < n; row++ {
			if v := math.Abs(a[row][col]); v > best {
				best, pivot = v, row
			}
		}
		if best < 1e-12 {
			return false
		}
		if pivot != col {
			a[col], a[pivot] = a[pivot], a[col]
		}
		inv := 1.0 / a[col][col]
		for c := col; c <= rhsCol; c++ {
			a[col][c] *= inv
		}
		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			f := a[row][col]
			if f == 0 {
				continue
			}
			for c := col; c <= rhsCol; c++ {
				a[row][c] -= f * a[col][c]
			}
		}
	}
	for i := 0; i < n; i++ {
		out[i] = a[i][rhsCol]
	}
	return true
}

// Predict returns the fitted value for one feature vector.
func (r *Ridge) Predict(x *[FeatureCount]float64) float64 {
	v := r.Bias
	for j := 0; j < FeatureCount; j++ {
		v += r.Weights[j] * x[j]
	}
	return v
}

// Coefs returns the used coefficients with their feature indices, for reports.
func (r *Ridge) Coefs(idx []int) map[int]float64 {
	out := make(map[int]float64, len(idx))
	for _, j := range idx {
		out[j] = r.Weights[j]
	}
	return out
}

// solve performs Gauss-Jordan elimination with partial pivoting on the
// augmented system a (n rows, width cols, last column is the right-hand side).
// Retained for backward compatibility with external calls and tests.
func solve(a [][FeatureCount + 1]float64, n, rhsCol int) ([]float64, bool) {
	var aFixed [FeatureCount + 1][FeatureCount + 1]float64
	for i := 0; i < n && i <= FeatureCount; i++ {
		copy(aFixed[i][:], a[i][:])
	}
	var outFixed [FeatureCount + 1]float64
	if !solveFixed(&aFixed, n, rhsCol, &outFixed) {
		return nil, false
	}
	out := make([]float64, n)
	copy(out, outFixed[:n])
	return out, true
}
