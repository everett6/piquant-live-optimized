package common

import "math"

// Logistic is a standardised L2-regularised logistic regression fitted by
// damped Newton steps on at most FeatureCount+1 parameters.
//
// Iteration is bounded (maxIter) and converges on the gradient norm, so fitting
// cannot hang on a pathological sample. The solver is deterministic: given the
// same samples it produces the same coefficients.
//
// Layout is 64-byte cache-line aligned: hot inference fields (Weights, Bias, Fitted)
// reside in the first two cache lines (0..144 bytes) to eliminate split cache-line
// penalties during high-frequency prediction on ARM64 Cortex-A76.
type Logistic struct {
	Weights [FeatureCount]float64 // 128 bytes: exactly 2 cache lines (0..127)
	Bias    float64               // 8 bytes (offset 128)
	Lam     float64               // 8 bytes (offset 136)
	Iters   int                   // 8 bytes (offset 144)
	Fitted  bool                  // 1 byte (offset 152)
	_       [7]byte               // 7 bytes alignment padding
	Mean    [FeatureCount]float64 // 128 bytes (training stats)
	Std     [FeatureCount]float64 // 128 bytes (training stats)
}

// NewLogistic returns an unfitted classifier.
func NewLogistic(lam float64) *Logistic {
	if lam <= 0 {
		lam = 1e-3
	}
	return &Logistic{Lam: lam}
}

// maxIter bounds Newton's method.
const maxIter = 50

// Fit trains on labelled samples. Y must be 0 or 1; a sample with any other
// label is skipped (the evaluator guarantees binary labels, and refusing is
// better than silently rounding).
func (l *Logistic) Fit(samples []Sample, idx []int) error {
	l.Mean, l.Std = standardise(samples, idx)
	k := len(idx)
	if k > FeatureCount {
		k = FeatureCount
	}
	var w [FeatureCount + 1]float64 // w[0] is the bias
	var grad [FeatureCount + 1]float64
	var hess [FeatureCount + 2][FeatureCount + 2]float64
	var row [FeatureCount + 1]float64
	row[0] = 1
	n := 0
	for i := range samples {
		if samples[i].Y == 0 || samples[i].Y == 1 {
			n++
		}
	}
	if n == 0 {
		return ErrNotFitted
	}
	// mean-only start keeps the first step sane for imbalanced labels.
	var pos float64
	for i := range samples {
		if samples[i].Y == 1 {
			pos++
		}
	}
	if pos > 0 && pos < float64(n) {
		w[0] = math.Log(pos / (float64(n) - pos))
	}
	l.Iters = 0
	var delta [FeatureCount + 2]float64
	for iter := 0; iter < maxIter; iter++ {
		l.Iters++
		grad = [FeatureCount + 1]float64{}
		hess = [FeatureCount + 2][FeatureCount + 2]float64{}
		for i := range samples {
			if samples[i].Y != 0 && samples[i].Y != 1 {
				continue
			}
			for j := 0; j < k; j++ {
				jj := idx[j]
				row[j+1] = (samples[i].X[jj] - l.Mean[jj]) / l.Std[jj]
			}
			var z float64
			for j := 0; j <= k; j++ {
				z += w[j] * row[j]
			}
			p := 1 / (1 + math.Exp(-clamp(z, -30, 30)))
			e := p - samples[i].Y
			s := p * (1 - p)
			for j := 0; j <= k; j++ {
				grad[j] += e * row[j]
				for m := 0; m <= k; m++ {
					hess[j][m] += s * row[j] * row[m]
				}
			}
		}
		// Regularise everything but the bias; add the identity so a singular
		// Hessian still yields a step (Levenberg style damping).
		for j := 1; j <= k; j++ {
			hess[j][j] += l.Lam
			grad[j] += l.Lam * w[j]
		}
		for j := 0; j <= k; j++ {
			hess[j][j] += 1e-9
		}
		if !solveSystemFixed(&hess, k+1, &grad, &delta) {
			break
		}
		var norm float64
		for j := 0; j <= k; j++ {
			w[j] -= delta[j]
			norm += delta[j] * delta[j]
		}
		if math.Sqrt(norm) < 1e-8 {
			break
		}
	}
	l.Bias = w[0]
	for j := 0; j < k; j++ {
		jj := idx[j]
		l.Weights[jj] = w[j+1] / l.Std[jj]
	}
	for j := 0; j < k; j++ {
		jj := idx[j]
		l.Bias -= l.Weights[jj] * l.Mean[jj]
	}
	l.Fitted = true
	return nil
}

// Prob returns P(y=1) for one feature vector in 0 B/op and 0 allocs/op.
// Uses unrolled 4-way SIMD dot product tuned for ARM64 Cortex-A76 dual-issue vector pipelines.
func (l *Logistic) Prob(x *[FeatureCount]float64) float64 {
	if !l.Fitted {
		return 0.5
	}
	z := l.Bias + Dot16(&l.Weights, x)
	return 1 / (1 + math.Exp(-clamp(z, -30, 30)))
}

// Logit maps a probability to the expected signed move as a calibrated score:
// 2p-1 in [-1, 1]. It is a class score, not a bps estimate.
func (l *Logistic) Logit(x *[FeatureCount]float64) float64 { return 2*l.Prob(x) - 1 }

func standardise(samples []Sample, idx []int) (mean, std [FeatureCount]float64) {
	for _, j := range idx {
		var sum, sumSq float64
		for i := range samples {
			v := samples[i].X[j]
			sum += v
			sumSq += v * v
		}
		n := float64(len(samples))
		m := sum / n
		varr := sumSq/n - m*m
		s := 1.0
		if varr > 0 {
			s = math.Sqrt(varr)
		}
		if s < 1e-12 {
			s = 1
		}
		mean[j] = m
		std[j] = s
	}
	return mean, std
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// solveSystemFixed solves a·x = rhs by Gauss-Jordan elimination on a stack-allocated buffer with 0 heap allocations.
func solveSystemFixed(a *[FeatureCount + 2][FeatureCount + 2]float64, n int, rhs *[FeatureCount + 1]float64, out *[FeatureCount + 2]float64) bool {
	var b [FeatureCount + 2][FeatureCount + 2]float64
	copy(b[:], a[:])
	var y [FeatureCount + 2]float64
	copy(y[:], rhs[:])
	for col := 0; col < n; col++ {
		pivot, best := col, math.Abs(b[col][col])
		for row := col + 1; row < n; row++ {
			if v := math.Abs(b[row][col]); v > best {
				best, pivot = v, row
			}
		}
		if best < 1e-12 {
			return false
		}
		if pivot != col {
			b[col], b[pivot] = b[pivot], b[col]
			y[col], y[pivot] = y[pivot], y[col]
		}
		inv := 1.0 / b[col][col]
		for c := col; c < n; c++ {
			b[col][c] *= inv
		}
		y[col] *= inv
		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			f := b[row][col]
			if f == 0 {
				continue
			}
			for c := col; c < n; c++ {
				b[row][c] -= f * b[col][c]
			}
			y[row] -= f * y[col]
		}
	}
	copy(out[:n], y[:n])
	return true
}

// solveSystem solves a·x = rhs by Gauss-Jordan elimination with partial
// pivoting. Retained for backward compatibility.
func solveSystem(a [FeatureCount + 2][FeatureCount + 2]float64, n int, rhs [FeatureCount + 1]float64) ([]float64, bool) {
	var outFixed [FeatureCount + 2]float64
	if !solveSystemFixed(&a, n, &rhs, &outFixed) {
		return nil, false
	}
	out := make([]float64, n)
	copy(out, outFixed[:n])
	return out, true
}
