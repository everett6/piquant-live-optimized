package common

import (
	"math"
)

// KylesLambdaEstimator computes the Kyle (1985) microstructural price impact coefficient:
//
//	\lambda_{Kyle} = \frac{\operatorname{Cov}(\Delta P, Q_{\text{signed}})}{\operatorname{Var}(Q_{\text{signed}}) + \epsilon}
//
// and estimates temporary/permanent market impact and maximum maker quote sizing under
// an adverse selection impact budget using recursive online EWMA covariance and variance tracking.
//
// All methods execute on the hot path in 0 B/op and 0 allocs/op.
type KylesLambdaEstimator struct {
	Alpha         float64 // EWMA forgetting/decay factor in (0, 1] (default: 0.05)
	Epsilon       float64 // Denominator stabilizer \epsilon (default: 1e-6)
	DefaultLambda float64 // Fallback/prior lambda in bps/BTC (default: 0.25)
	PermFraction  float64 // Fraction of impact that is permanent vs temporary (default: 0.50)

	MeanQ     float64 // EWMA mean signed order size
	MeanDP    float64 // EWMA mean price change in bps
	VarQ      float64 // EWMA variance of signed order size
	CovDPQ    float64 // EWMA covariance between \Delta P and Q_{\text{signed}}
	LastPrice float64 // Previous trade print price
	Count     int64   // Number of trades processed
}

// NewKylesLambdaEstimator initializes a Kyle's Lambda estimator with decay factor alpha
// and default baseline lambda.
func NewKylesLambdaEstimator(alpha, defaultLambda float64) *KylesLambdaEstimator {
	if alpha <= 0 || alpha > 1.0 {
		alpha = 0.05
	}
	if defaultLambda <= 0 {
		defaultLambda = 0.25
	}
	return &KylesLambdaEstimator{
		Alpha:         alpha,
		Epsilon:       1e-6,
		DefaultLambda: defaultLambda,
		PermFraction:  0.50,
		VarQ:          1.0,
		CovDPQ:        defaultLambda,
	}
}

// UpdateTrade processes a trade print: trade price, size, and aggressor side indicator
// (isBuyerMaker: true if buyer was maker, meaning incoming aggressor was a seller;
// false if seller was maker, meaning incoming aggressor was a buyer).
//
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (k *KylesLambdaEstimator) UpdateTrade(price, size float64, isBuyerMaker bool) {
	if price <= 0 || size <= 0 {
		return
	}
	if k.LastPrice <= 0 {
		k.LastPrice = price
		k.Count++
		return
	}

	// Price return in basis points: ((P_t - P_{t-1}) / P_{t-1}) * 10,000
	deltaP := ((price - k.LastPrice) / k.LastPrice) * 10000.0

	// Signed order size: positive for aggressive buy, negative for aggressive sell
	signedQ := size
	if isBuyerMaker {
		signedQ = -size
	}

	alpha := k.Alpha
	if alpha <= 0 || alpha > 1.0 {
		alpha = 0.05
	}

	if k.Count <= 1 {
		k.MeanQ = signedQ
		k.MeanDP = deltaP
		k.VarQ = signedQ * signedQ
		if k.VarQ < 1e-4 {
			k.VarQ = 1e-4
		}
		k.CovDPQ = deltaP * signedQ
	} else {
		// Recursive online EWMA covariance and variance tracking
		diffQ := signedQ - k.MeanQ
		diffDP := deltaP - k.MeanDP

		k.MeanQ += alpha * diffQ
		k.MeanDP += alpha * diffDP

		diffQPost := signedQ - k.MeanQ
		diffDPPost := deltaP - k.MeanDP

		k.VarQ = (1.0-alpha)*k.VarQ + alpha*(diffQ*diffQPost)
		k.CovDPQ = (1.0-alpha)*k.CovDPQ + alpha*(diffQ*diffDPPost)
	}

	k.LastPrice = price
	k.Count++
}

// Lambda returns the estimated Kyle's Lambda microstructural price impact coefficient (bps per unit volume).
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (k *KylesLambdaEstimator) Lambda() float64 {
	defLam := k.DefaultLambda
	if defLam <= 0 {
		defLam = 0.25
	}
	if k.Count < 3 {
		return defLam
	}
	eps := k.Epsilon
	if eps <= 0 {
		eps = 1e-6
	}
	denom := k.VarQ + eps
	if denom <= 0 {
		return defLam
	}
	lam := k.CovDPQ / denom
	// Microstructural price impact is non-negative; floor at small positive value
	if lam < 0.01 {
		return 0.01
	}
	// Bound against extreme single-tick outliers
	if lam > 50.0 {
		return 50.0
	}
	return lam
}

// Impact calculates temporary, permanent, and total adverse price impact (in bps) for a desired order size Q.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (k *KylesLambdaEstimator) Impact(size float64) (tempImpact, permImpact, totalImpact float64) {
	if size <= 0 {
		return 0, 0, 0
	}
	lam := k.Lambda()
	totalImpact = lam * size

	frac := k.PermFraction
	if frac <= 0 || frac > 1.0 {
		frac = 0.50
	}
	permImpact = totalImpact * frac
	tempImpact = totalImpact * (1.0 - frac)
	return tempImpact, permImpact, totalImpact
}

// OptimalSize computes the maximum maker quote size such that estimated adverse price
// impact remains under user-defined budgetBps.
//
//	Q_{opt} = \frac{\text{budgetBps}}{\lambda_{\text{Kyle}}}
//
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (k *KylesLambdaEstimator) OptimalSize(budgetBps, midPrice float64) float64 {
	if budgetBps <= 0 {
		return 0
	}
	lam := k.Lambda()
	if lam <= 0 {
		lam = 0.25
	}
	maxSize := budgetBps / lam
	return maxSize
}

// Reset clears the estimator's running statistics.
func (k *KylesLambdaEstimator) Reset() {
	k.MeanQ = 0
	k.MeanDP = 0
	k.VarQ = 1.0
	k.CovDPQ = k.DefaultLambda
	k.LastPrice = 0
	k.Count = 0
}

// KylesLambdaModel integrates Kyle's Lambda price impact estimation with ContinuousModel
// for research sweeps, ablation gate, and walk-forward CV.
type KylesLambdaModel struct {
	BaseRidge Ridge
	Alpha     float64
	fitted    bool
}

// NewKylesLambdaModel constructs an unfitted KylesLambdaModel.
func NewKylesLambdaModel(lambda, alpha float64) *KylesLambdaModel {
	if lambda <= 0 {
		lambda = 1e-3
	}
	if alpha <= 0 || alpha > 1.0 {
		alpha = 0.05
	}
	return &KylesLambdaModel{
		BaseRidge: Ridge{Lam: lambda},
		Alpha:     alpha,
	}
}

// Fit trains the base Ridge weights on the training samples.
func (m *KylesLambdaModel) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	useIdx := idx
	if len(useIdx) == 0 {
		useIdx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := m.BaseRidge.Fit(samples, useIdx); err != nil {
		return err
	}
	m.fitted = true
	return nil
}

// Predict calculates forward alpha forecast in bps from the 16-element core feature vector,
// scaled by Kyle's Lambda impact penalty.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (m *KylesLambdaModel) Predict(x *[FeatureCount]float64) float64 {
	baseAlpha := 0.0
	if m.fitted {
		baseAlpha = m.BaseRidge.Predict(x)
	}

	// Dynamic price impact proxy from order flow imbalance and return volatility:
	// lambda_est = |ret_1s_bps| / max(|ofi_1s|, 0.35)
	absFlow := math.Max(math.Abs(x[5]), 0.35)
	deltaP := math.Abs(x[11])
	if deltaP < 0.05 {
		deltaP = math.Abs(x[3]) * math.Max(0.2, x[4])
	}
	lambdaEst := deltaP / absFlow
	if lambdaEst > 10.0 {
		lambdaEst = 10.0
	}

	// Under high price impact regimes, scale down conviction to avoid execution drag
	impactPenalty := 1.0 / (1.0 + 0.12*lambdaEst)
	return baseAlpha * impactPenalty
}
