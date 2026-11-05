package common

import "math"

// MaxEnsembleExperts is the maximum number of quantitative experts tracked by
// AdaHedgeEnsemble in fixed stack-friendly arrays without heap allocations.
const MaxEnsembleExperts = 8

// DefaultExpertNames lists the 8 canonical microstructure model experts in PiQuant.
var DefaultExpertNames = [MaxEnsembleExperts]string{
	"mlofi_ridge",
	"hawkes_ofi",
	"glft_mm",
	"regime_ensemble",
	"perp_liquidation_reversion",
	"rough_signature_ofi",
	"elastic_net",
	"adaptive_rls",
}

// AdaHedgeEnsemble implements the adaptive Multiplicative Weights / AdaHedge
// regret-bounded meta-portfolio algorithm (de Rooij, van Erven, Grünwald, Koolen)
// over up to MaxEnsembleExperts (8) model experts.
//
// It tracks cumulative and exponentially decayed mixability gap (\Delta_t = h_t - m_t)
// and regret across experts, dynamically tuning the learning rate \eta_t = \ln(K) / \Delta_t
// with zero heap allocations (0 B/op, 0 allocs/op) in Combine and UpdateAndCombine.
type AdaHedgeEnsemble struct {
	NumExperts int                         `json:"num_experts"`
	Decay      float64                     `json:"decay"`       // Exponential forgetting factor \gamma \in (0, 1]
	CumLoss    [MaxEnsembleExperts]float64 `json:"cum_loss"`    // Decayed cumulative loss L_{k,t} per expert
	CumRegret  [MaxEnsembleExperts]float64 `json:"cum_regret"`  // Cumulative regret R_{k,t} vs meta-portfolio
	Weights    [MaxEnsembleExperts]float64 `json:"weights"`     // Current normalized simplex weights w_{k,t}
	CumMixGap  float64                     `json:"cum_mix_gap"` // Cumulative mixability gap \Delta_t = \sum (h_s - m_s)
	Eta        float64                     `json:"eta"`         // Adaptive learning rate \eta_t = \ln(K) / \Delta_t
	Steps      int64                       `json:"steps"`
}

// NewAdaHedgeEnsemble initializes an AdaHedge meta-portfolio for nExperts (up to 8)
// with uniform initial weights and exponential loss decay.
func NewAdaHedgeEnsemble(nExperts int, decay float64) *AdaHedgeEnsemble {
	if nExperts <= 0 || nExperts > MaxEnsembleExperts {
		nExperts = MaxEnsembleExperts
	}
	if decay <= 0 || decay > 1.0 {
		decay = 0.985
	}
	ens := &AdaHedgeEnsemble{
		NumExperts: nExperts,
		Decay:      decay,
		CumMixGap:  0.10,
		Eta:        math.Log(float64(nExperts)) / 0.10,
	}
	u := 1.0 / float64(nExperts)
	for k := 0; k < nExperts; k++ {
		ens.Weights[k] = u
	}
	return ens
}

// Combine computes the weighted meta-forecast \hat{y}_t = \sum_{k=0}^{K-1} w_{k,t} \hat{y}_{k,t}
// in 0 B/op and 0 allocs/op.
func (a *AdaHedgeEnsemble) Combine(expertPreds *[MaxEnsembleExperts]float64) float64 {
	n := a.NumExperts
	if n <= 0 || n > MaxEnsembleExperts {
		n = MaxEnsembleExperts
	}
	var out float64
	for k := 0; k < n; k++ {
		out += a.Weights[k] * expertPreds[k]
	}
	return out
}

// UpdateAndCombine updates the AdaHedge expert weights from the realized target outcome
// of the previous step (prevActual) and previous expert predictions (prevPreds), updates
// the cumulative mixability gap \Delta_t and adaptive learning rate \eta_t, and returns
// the combined forecast for currPreds along with the updated weight vector in 0 allocs/op.
func (a *AdaHedgeEnsemble) UpdateAndCombine(prevActual float64, prevPreds, currPreds [MaxEnsembleExperts]float64) (combined float64, weights [MaxEnsembleExperts]float64) {
	n := a.NumExperts
	if n <= 0 || n > MaxEnsembleExperts {
		n = MaxEnsembleExperts
	}
	decay := a.Decay
	if decay <= 0 || decay > 1.0 {
		decay = 0.985
	}

	// Ensure weights are initialized if struct was zero-constructed
	var wCheck float64
	for k := 0; k < n; k++ {
		wCheck += a.Weights[k]
	}
	if wCheck <= 1e-12 {
		u := 1.0 / float64(n)
		for k := 0; k < n; k++ {
			a.Weights[k] = u
		}
	}

	// Compute bounded Huber/absolute loss l_{k,t} for each expert and hedge loss h_t = \sum w_k l_k
	var losses [MaxEnsembleExperts]float64
	var hedgeLoss float64
	minLoss := math.MaxFloat64

	for k := 0; k < n; k++ {
		err := math.Abs(prevActual - prevPreds[k])
		// Bounded loss in [0, 25] bps for numerical stability
		if err > 25.0 {
			err = 25.0
		}
		losses[k] = err
		hedgeLoss += a.Weights[k] * err
		if err < minLoss {
			minLoss = err
		}
	}

	eta := a.Eta
	if eta <= 1e-6 || eta > 10.0 {
		eta = math.Log(float64(n)) / math.Max(0.10, a.CumMixGap)
	}

	// Compute mixLoss m_t = -(1/\eta) \ln(\sum_k w_{k,t} \exp(-\eta (l_{k,t} - minLoss))) + minLoss
	var sumExp float64
	for k := 0; k < n; k++ {
		sumExp += a.Weights[k] * math.Exp(-eta*(losses[k]-minLoss))
	}
	mixLoss := minLoss
	if sumExp > 1e-15 {
		mixLoss = minLoss - math.Log(sumExp)/eta
	}

	// Mixability gap \delta_t = \max(0, h_t - m_t)
	deltaGap := hedgeLoss - mixLoss
	if deltaGap < 0 {
		deltaGap = 0
	}
	a.CumMixGap = decay*a.CumMixGap + deltaGap + 1e-4

	// Adaptive AdaHedge learning rate \eta_{t+1} = \ln(K) / \Delta_t
	logK := math.Log(float64(n))
	a.Eta = math.Max(0.01, math.Min(5.0, logK/math.Max(0.05, a.CumMixGap)))

	// Update cumulative losses, regrets, and multiplicative weights
	minCumLoss := math.MaxFloat64
	for k := 0; k < n; k++ {
		a.CumLoss[k] = decay*a.CumLoss[k] + losses[k]
		a.CumRegret[k] = decay*a.CumRegret[k] + (hedgeLoss - losses[k])
		if a.CumLoss[k] < minCumLoss {
			minCumLoss = a.CumLoss[k]
		}
	}

	var sumW float64
	for k := 0; k < n; k++ {
		w := math.Exp(-a.Eta * (a.CumLoss[k] - minCumLoss))
		a.Weights[k] = w
		sumW += w
	}
	if sumW > 1e-15 {
		invSum := 1.0 / sumW
		for k := 0; k < n; k++ {
			a.Weights[k] *= invSum
		}
	} else {
		u := 1.0 / float64(n)
		for k := 0; k < n; k++ {
			a.Weights[k] = u
		}
	}

	a.Steps++
	for k := 0; k < n; k++ {
		combined += a.Weights[k] * currPreds[k]
	}
	return combined, a.Weights
}
