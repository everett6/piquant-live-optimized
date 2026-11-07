package common

import (
	"math"
)

// MarketMicroRegime classifies the prevailing order book microstructure regime.
type MarketMicroRegime uint8

const (
	RegimeLowVolRange MarketMicroRegime = iota
	RegimeTrending
	RegimeHighVolCascade
)

// NumMicroRegimes is the count of distinct microstructure regimes tracked.
const NumMicroRegimes = 3

// String returns the canonical uppercase regime name.
func (r MarketMicroRegime) String() string {
	switch r {
	case RegimeLowVolRange:
		return "LOW_VOL"
	case RegimeTrending:
		return "TRENDING"
	case RegimeHighVolCascade:
		return "CASCADE"
	default:
		return "UNKNOWN"
	}
}

// NumMetaExperts is the fixed number of quantitative alpha expert families in the ensemble.
const NumMetaExperts = 6

const (
	ExpertGLFTQueue uint8 = iota
	ExpertOBIMomentum
	ExpertLeadLag
	ExpertMeanReversion
	ExpertVPINDefense
	ExpertHawkesCascade
)

// MetaExpertNames provides static strings for each alpha expert family without heap allocations.
var MetaExpertNames = [NumMetaExperts]string{
	"glft_queue",
	"obi_momentum",
	"lead_lag",
	"mean_reversion",
	"vpin_defense",
	"hawkes_cascade",
}

// AdaptiveMetaEnsemble implements an online Multiplicative Weights / Discounted Hedge
// Meta-Learner combining signals across 6 alpha families conditioned on real-time market regimes.
//
// All hot paths (Predict, UpdateEdge, Regime, TopExpert) execute in 0 B/op and 0 allocs/op.
type AdaptiveMetaEnsemble struct {
	Gamma float64 // Discount / forgetting factor \gamma \in (0, 1] (default: 0.98)
	Eta   float64 // Online learning rate \eta > 0 (default: 0.15)

	// Per-regime discounted expert weights: Weights[regime][expert]
	Weights [NumMicroRegimes][NumMetaExperts]float64

	// Real-time market state sensors
	CurrentRegime  MarketMicroRegime
	Volatility     float64
	JumpRatio      float64
	BranchingRatio float64

	// Base ridge model for sample fitting if required
	BaseRidge Ridge
	fitted    bool
}

// NewAdaptiveMetaEnsemble constructs an unfitted AdaptiveMetaEnsemble with uniform
// initial expert weights across all regimes.
func NewAdaptiveMetaEnsemble(lambda, gamma, eta float64) *AdaptiveMetaEnsemble {
	if lambda <= 0 {
		lambda = 1e-3
	}
	if gamma <= 0 || gamma > 1.0 {
		gamma = 0.98
	}
	if eta <= 0 {
		eta = 0.15
	}

	ens := &AdaptiveMetaEnsemble{
		Gamma:     gamma,
		Eta:       eta,
		BaseRidge: Ridge{Lam: lambda},
	}

	// Initialize uniform probability simplex weights w_{r,i} = 1 / K
	u := 1.0 / float64(NumMetaExperts)
	for r := 0; r < NumMicroRegimes; r++ {
		for i := 0; i < NumMetaExperts; i++ {
			ens.Weights[r][i] = u
		}
	}

	return ens
}

// ClassifyMarketRegime classifies market microstructure into 3 regimes based on
// realized volatility (bps), jump ratio (ratio of jump variance to total return variance or |ret|/vol),
// and Hawkes branching ratio (n = \alpha / \beta).
func ClassifyMarketRegime(volBps, jumpRatio, branchingRatio float64) MarketMicroRegime {
	if branchingRatio >= 0.85 || jumpRatio >= 2.0 || volBps >= 4.0 {
		return RegimeHighVolCascade
	}
	if volBps >= 1.5 || jumpRatio >= 1.0 {
		return RegimeTrending
	}
	return RegimeLowVolRange
}

// UpdateSensors updates the regime sensors and dynamically classifies the regime.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (a *AdaptiveMetaEnsemble) UpdateSensors(volBps, jumpRatio, branchingRatio float64) {
	a.Volatility = volBps
	a.JumpRatio = jumpRatio
	a.BranchingRatio = branchingRatio
	a.CurrentRegime = ClassifyMarketRegime(volBps, jumpRatio, branchingRatio)
}

// Regime returns the current classified market microstructure regime.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (a *AdaptiveMetaEnsemble) Regime() MarketMicroRegime {
	return a.CurrentRegime
}

// TopExpert returns the ID, name, and weight of the top-weighted expert in regime r.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (a *AdaptiveMetaEnsemble) TopExpert(r MarketMicroRegime) (expertID uint8, name string, weight float64) {
	if r >= NumMicroRegimes {
		r = RegimeLowVolRange
	}
	bestID := uint8(0)
	bestW := a.Weights[r][0]
	for i := 1; i < NumMetaExperts; i++ {
		if a.Weights[r][i] > bestW {
			bestW = a.Weights[r][i]
			bestID = uint8(i)
		}
	}
	return bestID, MetaExpertNames[bestID], bestW
}

// UpdateEdge updates the online expert weights for regime r via exponential multiplicative updates:
//
//	w_{r,i}^{(t+1)} = \frac{w_{r,i}^{(t)\gamma} \cdot \exp(\eta \cdot R_{i,t})}{Z_t}
//
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (a *AdaptiveMetaEnsemble) UpdateEdge(r MarketMicroRegime, rewards [NumMetaExperts]float64) {
	if r >= NumMicroRegimes {
		r = RegimeLowVolRange
	}
	gamma := a.Gamma
	if gamma <= 0 || gamma > 1.0 {
		gamma = 0.98
	}
	eta := a.Eta
	if eta <= 0 {
		eta = 0.15
	}

	var sumZ float64
	var newW [NumMetaExperts]float64

	for i := 0; i < NumMetaExperts; i++ {
		w := a.Weights[r][i]
		if w <= 1e-12 {
			w = 1e-12
		}
		// w_{r,i}^{(t)\gamma} * exp(\eta * R_{i,t})
		val := math.Pow(w, gamma) * math.Exp(eta*rewards[i])
		if math.IsNaN(val) || math.IsInf(val, 0) || val < 1e-12 {
			val = 1e-12
		}
		newW[i] = val
		sumZ += val
	}

	if sumZ > 0 {
		invZ := 1.0 / sumZ
		for i := 0; i < NumMetaExperts; i++ {
			a.Weights[r][i] = newW[i] * invZ
		}
	}
}

// ComputeExpertSignals evaluates each of the 6 alpha expert families on feature vector x.
// Returns predictions in bps with 0 B/op, 0 allocs/op.
func (a *AdaptiveMetaEnsemble) ComputeExpertSignals(x *[FeatureCount]float64) [NumMetaExperts]float64 {
	var sigs [NumMetaExperts]float64

	// Feature layout reference:
	// x[0]: imb_l1, x[3]: micro_dev, x[4]: spread_bps, x[5]: ofi_1s, x[6]: ofi_5s,
	// x[8]: mlofi_1s, x[10]: vol_30s_bps, x[11]: ret_1s_bps, x[12]: ret_5s_bps,
	// x[13]: trade_imb_5s, x[14]: trade_cnt_1s, x[15]: queue_ratio

	// 1. GLFT / Queue-Reactive expert
	// Responsive to L1 imbalance, queue depth ratio, and microprice deviation
	spread := math.Max(0.1, x[4])
	sigs[ExpertGLFTQueue] = 0.45*x[0]*spread + 0.35*x[3]*spread + 0.20*(x[15]-0.5)*spread

	// 2. OBI / Momentum expert
	// Responsive to fast and medium order flow imbalance and 1s returns
	sigs[ExpertOBIMomentum] = 0.40*x[5] + 0.25*x[6] + 0.35*x[11]

	// 3. Cross-Venue Lead-Lag expert
	// Multi-level OFI innovation and short-term return acceleration
	sigs[ExpertLeadLag] = 0.55*x[8] + 0.45*(x[11]-0.5*x[12])

	// 4. Mean-Reversion / Basis expert
	// Microprice deviation fade and short-term return reversal
	sigs[ExpertMeanReversion] = -0.50*x[3]*spread - 0.30*x[11]

	// 5. Toxicity / VPIN Defense expert
	// Dampens adverse selection from heavy signed trade flow imbalances
	sigs[ExpertVPINDefense] = -0.25 * x[13] * spread

	// 6. Hawkes Cascade expert
	// Detects self-exciting trading clusters and follows cascade momentum
	tradeRate := math.Min(10.0, float64(x[14]))
	sigs[ExpertHawkesCascade] = 0.35 * x[11] * (1.0 + 0.10*tradeRate)

	return sigs
}

// Fit trains the underlying base Ridge model on historical samples to calibrate
// baseline linear parameters.
func (a *AdaptiveMetaEnsemble) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	useIdx := idx
	if len(useIdx) == 0 {
		useIdx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := a.BaseRidge.Fit(samples, useIdx); err != nil {
		return err
	}
	a.fitted = true
	return nil
}

// Predict combines signals from all alpha families according to the active regime
// and its online learned expert weights.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (a *AdaptiveMetaEnsemble) Predict(x *[FeatureCount]float64) float64 {
	// Dynamically update regime from current feature vector if sensors not fed externally
	r := a.CurrentRegime
	vol := x[10]
	if a.Volatility <= 0 && vol > 0 {
		jumpEst := 0.0
		if vol > 0.1 {
			jumpEst = math.Abs(x[11]) / vol
		}
		branchEst := math.Min(1.0, math.Abs(x[13])*0.4+float64(x[14])*0.04)
		r = ClassifyMarketRegime(vol, jumpEst, branchEst)
	}

	if r >= NumMicroRegimes {
		r = RegimeLowVolRange
	}

	sigs := a.ComputeExpertSignals(x)

	var ensemblePred float64
	for i := 0; i < NumMetaExperts; i++ {
		ensemblePred += a.Weights[r][i] * sigs[i]
	}

	if a.fitted {
		base := a.BaseRidge.Predict(x)
		return 0.70*ensemblePred + 0.30*base
	}

	return ensemblePred
}
