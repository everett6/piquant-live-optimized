package common

import (
	"math"
)

// BasisAction defines the statistical arbitrage decision emitted by SpotPerpBasisModel.
type BasisAction string

const (
	ActionConvergenceBuySpotSellPerp BasisAction = "CONVERGENCE_BUY_SPOT_SELL_PERP"
	ActionConvergenceSellSpotBuyPerp BasisAction = "CONVERGENCE_SELL_SPOT_BUY_PERP"
	ActionHold                       BasisAction = "HOLD"
)

// BasisDecision contains the instantaneous basis metrics and arbitrage decision.
type BasisDecision struct {
	BasisBps  float64     `json:"basis_bps"`
	ZScore    float64     `json:"z_score"`
	AlphaBps  float64     `json:"alpha_bps"`
	SpreadBps float64     `json:"spread_bps"`
	Action    BasisAction `json:"action"`
}

// SpotPerpBasisModel tracks spot vs. perpetual futures price divergence,
// maintains an online rolling Welford EWMA of mean basis and basis volatility,
// standardizes the basis into a Z-score, and calculates a mean-reverting basis alpha.
//
// All evaluation and prediction routines operate with zero heap allocations (0 B/op, 0 allocs/op).
type SpotPerpBasisModel struct {
	Theta      float64 // Mean-reversion speed: alpha_basis = -theta * Z
	ZThreshold float64 // Entry threshold Z-score for arbitrage convergence (default 1.5)
	Eta        float64 // EWMA update weight in (0, 1)

	// Rolling state
	SpotPrice   float64
	PerpPrice   float64
	MeanBasis   float64
	VarBasis    float64
	initialized bool

	// Baseline continuous predictor for experiment runner & feature scoring
	BaseRidge Ridge
	fitted    bool
	BasisWt   float64
}

// NewSpotPerpBasisModel creates an instance of SpotPerpBasisModel.
func NewSpotPerpBasisModel(lambda, theta, zThreshold float64) *SpotPerpBasisModel {
	if theta <= 0 {
		theta = 0.65
	}
	if zThreshold <= 0 {
		zThreshold = 1.5
	}
	return &SpotPerpBasisModel{
		Theta:      theta,
		ZThreshold: zThreshold,
		Eta:        0.05,
		VarBasis:   1.0,
		BaseRidge:  Ridge{Lam: lambda},
		BasisWt:    0.35,
	}
}

// Update updates the rolling spot and perp prices and returns the latest BasisDecision.
// Executes in 0 B/op and 0 allocs/op.
func (m *SpotPerpBasisModel) Update(spotPrice, perpPrice float64) BasisDecision {
	return m.Evaluate(spotPrice, perpPrice, 0.0)
}

// Evaluate updates the rolling Welford EWMA basis state with current spot, perp, and spread,
// and returns the full BasisDecision with zero heap allocations (0 B/op, 0 allocs/op).
func (m *SpotPerpBasisModel) Evaluate(spotPrice, perpPrice, spreadBps float64) BasisDecision {
	m.SpotPrice = spotPrice
	m.PerpPrice = perpPrice
	if spotPrice <= 0 {
		return BasisDecision{Action: ActionHold, SpreadBps: spreadBps}
	}

	// Basis in bps: ((F_t - S_t) / S_t) * 10,000
	basisBps := (perpPrice - spotPrice) / spotPrice * 10000.0

	// Welford EWMA update of rolling mean basis and basis variance
	if !m.initialized {
		m.MeanBasis = basisBps
		m.VarBasis = math.Max(0.25, basisBps*basisBps*0.1)
		m.initialized = true
	} else {
		delta := basisBps - m.MeanBasis
		m.MeanBasis += m.Eta * delta
		delta2 := basisBps - m.MeanBasis
		m.VarBasis = (1.0-m.Eta)*m.VarBasis + m.Eta*(delta*delta2)
		if m.VarBasis < 1e-4 {
			m.VarBasis = 1e-4
		}
	}

	sigma := math.Sqrt(m.VarBasis)
	z := (basisBps - m.MeanBasis) / sigma
	if z > 10.0 {
		z = 10.0
	} else if z < -10.0 {
		z = -10.0
	}

	// Mean-reverting basis alpha: alpha_basis = -theta * Z
	alphaBps := -m.Theta * z

	var action BasisAction = ActionHold
	// Rich basis (Perp > Spot dislocation beyond statistical threshold):
	// Convergence trade: Buy Spot, Sell Perp
	if z >= m.ZThreshold {
		action = ActionConvergenceBuySpotSellPerp
	} else if z <= -m.ZThreshold {
		// Cheap basis (Perp < Spot dislocation below statistical threshold):
		// Convergence trade: Sell Spot, Buy Perp
		action = ActionConvergenceSellSpotBuyPerp
	}

	return BasisDecision{
		BasisBps:  basisBps,
		ZScore:    z,
		AlphaBps:  alphaBps,
		SpreadBps: spreadBps,
		Action:    action,
	}
}

// EvaluateStateless computes the decision without mutating the model's EWMA state.
func (m *SpotPerpBasisModel) EvaluateStateless(spotPrice, perpPrice, spreadBps float64) BasisDecision {
	if spotPrice <= 0 {
		return BasisDecision{Action: ActionHold, SpreadBps: spreadBps}
	}
	basisBps := (perpPrice - spotPrice) / spotPrice * 10000.0
	mean := m.MeanBasis
	variance := m.VarBasis
	if !m.initialized {
		mean = basisBps
		variance = math.Max(0.25, basisBps*basisBps*0.1)
	}
	sigma := math.Sqrt(math.Max(1e-4, variance))
	z := (basisBps - mean) / sigma
	if z > 10.0 {
		z = 10.0
	} else if z < -10.0 {
		z = -10.0
	}
	alphaBps := -m.Theta * z

	var action BasisAction = ActionHold
	if z >= m.ZThreshold {
		action = ActionConvergenceBuySpotSellPerp
	} else if z <= -m.ZThreshold {
		action = ActionConvergenceSellSpotBuyPerp
	}

	return BasisDecision{
		BasisBps:  basisBps,
		ZScore:    z,
		AlphaBps:  alphaBps,
		SpreadBps: spreadBps,
		Action:    action,
	}
}

// Fit trains the underlying Ridge predictor and calibrates basis sensitivity on historical samples.
func (m *SpotPerpBasisModel) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	if len(idx) == 0 {
		idx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := m.BaseRidge.Fit(samples, idx); err != nil {
		return err
	}

	// Calibrate initial Welford basis statistics from feature distribution
	for i := range samples {
		x := &samples[i].X
		spread := math.Max(0.1, x[4])
		microBasis := x[3] * spread * 1.4
		ofiPersistence := 0.18*x[6] + 0.08*x[7] + 0.22*x[9]
		flowSkew := 0.35*x[13] + 0.15*x[12] + 0.40*(x[0]-x[2])
		synBasis := microBasis + ofiPersistence + flowSkew

		if !m.initialized {
			m.MeanBasis = synBasis
			m.VarBasis = math.Max(0.25, synBasis*synBasis)
			m.initialized = true
		} else {
			delta := synBasis - m.MeanBasis
			m.MeanBasis += m.Eta * delta
			delta2 := synBasis - m.MeanBasis
			m.VarBasis = (1.0-m.Eta)*m.VarBasis + m.Eta*(delta*delta2)
		}
	}

	m.fitted = true
	return nil
}

// Predict calculates forward alpha forecast in bps from the 16-element core feature vector.
// Operates in 0 B/op and 0 allocs/op.
func (m *SpotPerpBasisModel) Predict(x *[FeatureCount]float64) float64 {
	baseAlpha := 0.0
	if m.fitted {
		baseAlpha = m.BaseRidge.Predict(x)
	}

	spread := math.Max(0.1, x[4])
	microBasis := x[3] * spread * 1.4
	ofiPersistence := 0.18*x[6] + 0.08*x[7] + 0.22*x[9]
	flowSkew := 0.35*x[13] + 0.15*x[12] + 0.40*(x[0]-x[2])
	synBasis := microBasis + ofiPersistence + flowSkew

	sigma := math.Sqrt(math.Max(1e-4, m.VarBasis))
	z := (synBasis - m.MeanBasis) / sigma
	if z > 10.0 {
		z = 10.0
	} else if z < -10.0 {
		z = -10.0
	}

	basisAlpha := -m.Theta * z
	return baseAlpha + m.BasisWt*basisAlpha
}

// Clone returns an independent deep copy of the SpotPerpBasisModel.
func (m *SpotPerpBasisModel) Clone() *SpotPerpBasisModel {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}
