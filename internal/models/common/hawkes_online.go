package common

import (
	"math"
)

// OnlineHawkesEstimator implements a self-calibrating online Hawkes point-process
// intensity estimator and cascade detector.
//
// In an exponential Hawkes process:
//
//	lambda(t) = mu + sum_{t_i < t} alpha * exp(-beta * (t - t_i))
//
// Recursively evaluated on each trade event (t_k, isBuy, qty) in O(1) time:
//
//	R(t_k) = exp(-beta * (t_k - t_{k-1})) * R(t_{k-1}) + alpha * mark(qty)
//	lambda(t_k) = mu + R(t_k)
//
// Stability & Cascade Regime:
//
//	Branching ratio n = alpha / beta.
//	When n >= 0.90, the process enters a supercritical / self-exciting cascade regime
//	where trade arrivals cluster rapidly into forced liquidations or toxic sweeps.
//
// Operates on the hot path in 0 B/op and 0 allocs/op.
type OnlineHawkesEstimator struct {
	Mu    float64 // Baseline background intensity (arrivals/sec)
	Alpha float64 // Jump excitation magnitude
	Beta  float64 // Exponential decay rate (1/sec)

	// Recursive excitation state
	rBuy  float64
	rSell float64
	rTot  float64

	lastTimeNs   int64
	lastUpdateNs int64

	// Online self-calibration metrics
	tradeCount uint64
	empRate    float64 // Exponentially moving arrival rate (trades/sec)
	adaptRate  float64 // Learning rate for background intensity calibration
}

// NewOnlineHawkesEstimator creates an OnlineHawkesEstimator with specified parameters.
// If non-positive values are provided, calibrated defaults are used:
// mu = 5.0, alpha = 12.0, beta = 20.0 (branching ratio n = 0.60).
func NewOnlineHawkesEstimator(mu, alpha, beta float64) *OnlineHawkesEstimator {
	if mu <= 0 {
		mu = 5.0
	}
	if alpha <= 0 {
		alpha = 12.0
	}
	if beta <= 0 {
		beta = 20.0
	}
	return &OnlineHawkesEstimator{
		Mu:        mu,
		Alpha:     alpha,
		Beta:      beta,
		adaptRate: 0.02,
		empRate:   mu,
	}
}

// Update recursively ingests a trade event (t_k, isBuy, size) in O(1) time.
// Zero heap allocations (0 B/op, 0 allocs/op).
func (h *OnlineHawkesEstimator) Update(nowNs int64, isBuy bool, size float64) {
	if h.lastTimeNs <= 0 {
		h.lastTimeNs = nowNs
		h.lastUpdateNs = nowNs
		h.tradeCount = 1
		h.empRate = h.Mu
		return
	}

	dtSec := float64(nowNs-h.lastTimeNs) / 1e9
	if dtSec < 0 {
		dtSec = 0
	}

	// Exponential decay of accumulated excitation over elapsed interval
	decay := 1.0
	if dtSec > 0 {
		decay = math.Exp(-h.Beta * dtSec)
	}

	h.rBuy *= decay
	h.rSell *= decay
	h.rTot *= decay

	// Mark scaling: mild log-dampening for large institutional sizes
	mark := 1.0
	if size > 0 {
		mark = 1.0 + 0.1*math.Log1p(size)
	}
	jump := h.Alpha * mark

	if isBuy {
		h.rBuy += jump
	} else {
		h.rSell += jump
	}
	h.rTot += jump

	h.tradeCount++
	h.lastTimeNs = nowNs
	h.lastUpdateNs = nowNs

	// Self-calibration: in stationary Hawkes, E[lambda] = mu / (1 - alpha/beta)
	// Hence background mu can be smoothly adapted towards empRate * (1 - n)
	if dtSec > 0 && dtSec < 10.0 {
		instRate := 1.0 / dtSec
		if instRate > 250.0 {
			instRate = 250.0
		}
		gamma := h.adaptRate
		if gamma <= 0 {
			gamma = 0.02
		}
		h.empRate = (1.0-gamma)*h.empRate + gamma*instRate

		n := h.BranchingRatio()
		if n > 0.05 && n < 0.95 {
			targetMu := math.Max(0.5, h.empRate*(1.0-n))
			h.Mu = (1.0-gamma)*h.Mu + gamma*targetMu
		}
	}
}

// Intensity returns the instantaneous total arrival intensity lambda(t).
func (h *OnlineHawkesEstimator) Intensity() float64 {
	return h.Mu + h.rTot
}

// IntensityAt returns the decayed intensity at a hypothetical time nowNs without mutating state.
func (h *OnlineHawkesEstimator) IntensityAt(nowNs int64) float64 {
	if h.lastUpdateNs <= 0 || nowNs <= h.lastUpdateNs {
		return h.Intensity()
	}
	dtSec := float64(nowNs-h.lastUpdateNs) / 1e9
	decay := math.Exp(-h.Beta * dtSec)
	return h.Mu + h.rTot*decay
}

// BranchingRatio returns the self-excitation branching ratio n = alpha / beta.
func (h *OnlineHawkesEstimator) BranchingRatio() float64 {
	if h.Beta <= 0 {
		return 0
	}
	return h.Alpha / h.Beta
}

// SetBranchingRatio sets alpha such that alpha / beta == n.
func (h *OnlineHawkesEstimator) SetBranchingRatio(n float64) {
	if h.Beta > 0 && n >= 0 {
		h.Alpha = n * h.Beta
	}
}

// IsCascadeRisk reports whether the Hawkes system has entered a self-exciting cascade regime.
// Triggered when branching ratio n >= 0.90 or when excitation surges beyond safety limits.
func (h *OnlineHawkesEstimator) IsCascadeRisk() bool {
	return h.BranchingRatio() >= 0.90 || (h.rTot > 0 && (h.rTot/(h.Mu+1e-6)) >= 8.0)
}

// BuyIntensity returns the buy-side arrival intensity lambda_buy(t).
func (h *OnlineHawkesEstimator) BuyIntensity() float64 {
	return h.Mu*0.5 + h.rBuy
}

// SellIntensity returns the sell-side arrival intensity lambda_sell(t).
func (h *OnlineHawkesEstimator) SellIntensity() float64 {
	return h.Mu*0.5 + h.rSell
}

// FlowImbalance returns the directional trade intensity imbalance in [-1, 1].
func (h *OnlineHawkesEstimator) FlowImbalance() float64 {
	denom := h.rBuy + h.rSell
	if denom <= 1e-6 {
		return 0
	}
	return (h.rBuy - h.rSell) / denom
}

// Reset clears the excitation state while preserving calibrated parameters.
func (h *OnlineHawkesEstimator) Reset() {
	h.rBuy = 0
	h.rSell = 0
	h.rTot = 0
	h.lastTimeNs = 0
	h.lastUpdateNs = 0
}
