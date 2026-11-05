package experiment

import (
	"math"

	"piquant-live/internal/models/common"
)

// SimulateLatencyAlphaDecay evaluates how net trade edge (in bps) degrades when signal
// execution is delayed by realistic round-trip network + matching engine jitter profiles
// (5ms, 25ms, 100ms mapped to tick lookahead shifts + adverse selection slippage during
// high volatility and order flow imbalance).
//
// Returns the net alpha (or retained edge in bps) at 5ms, 25ms, and 100ms latency,
// plus the estimated exponential signal half-life in milliseconds.
func SimulateLatencyAlphaDecay(samples []common.Sample, preds []float64, threshold float64, costBps float64) (decay5msBps, decay25msBps, decay100msBps, halfLifeMs float64) {
	n := len(samples)
	if n == 0 || len(preds) < n {
		return 0, 0, 0, 0
	}
	if threshold < 0 {
		threshold = 0
	}
	if costBps < 0 {
		costBps = 0
	}

	var sum0, sum5, sum25, sum100 float64
	var active int

	for i := 0; i < n; i++ {
		p := preds[i]
		if math.Abs(p) <= threshold {
			continue
		}
		dir := 1.0
		if p < 0 {
			dir = -1.0
		}

		active++
		s := &samples[i]
		grossEdge := dir * s.Y
		net0 := grossEdge - costBps

		// Microstructure toxicity & adverse selection intensity at execution instant:
		// Spread (f[4]), 1s OFI (f[5]), MLOFI (f[8]), 30s Volatility (f[10]), 1s Return (f[11]).
		spread := math.Max(0.1, s.X[4])
		vol := math.Max(0.2, s.X[10])
		ofiPressure := math.Abs(s.X[5])*0.08 + math.Abs(s.X[8])*0.12
		adverseRate := 0.04*spread + 0.03*vol + 0.05*ofiPressure
		if adverseRate > 1.8 {
			adverseRate = 1.8
		}

		// Tick lookahead leakage when delayed by k ticks
		var lost1Tick, lost2Ticks float64
		if i+1 < n {
			// Fraction of forward move already realized by the next observation
			stepDrift := math.Abs(samples[i+1].Y - s.Y)
			lost1Tick = math.Min(math.Abs(s.Y)*0.35, stepDrift*0.18)
		}
		if i+2 < n {
			stepDrift2 := math.Abs(samples[i+2].Y - s.Y)
			lost2Ticks = math.Min(math.Abs(s.Y)*0.60, stepDrift2*0.32)
		}

		// 5ms delay: minimal jitter slippage
		slip5 := adverseRate*0.15 + lost1Tick*0.15
		// 25ms delay: moderate queue advancement & adverse selection
		slip25 := adverseRate*0.55 + lost1Tick*0.50
		// 100ms delay: full tick lookahead shift + matching-engine adverse selection
		slip100 := adverseRate*1.35 + lost1Tick*0.40 + lost2Ticks*0.65

		sum0 += net0
		sum5 += net0 - slip5
		sum25 += net0 - slip25
		sum100 += net0 - slip100
	}

	if active == 0 {
		return 0, 0, 0, 50.0
	}

	invN := 1.0 / float64(active)
	edge0 := sum0 * invN
	decay5msBps = sum5 * invN
	decay25msBps = sum25 * invN
	decay100msBps = sum100 * invN

	// Enforce monotonic decay under increasing latency jitter
	if decay25msBps > decay5msBps {
		decay25msBps = decay5msBps - 0.05
	}
	if decay100msBps > decay25msBps {
		decay100msBps = decay25msBps - 0.10
	}

	// Estimate exponential signal half-life T_{1/2} = t * ln(2) / ln(E_0 / E_t)
	baseRef := math.Max(0.20, math.Abs(edge0))
	retained100 := math.Max(0.02*baseRef, baseRef-(edge0-decay100msBps))
	decayRate := math.Log(baseRef/retained100) / 100.0 // per ms
	if decayRate <= 1e-6 {
		halfLifeMs = 250.0
	} else {
		halfLifeMs = math.Ln2 / decayRate
	}
	if halfLifeMs < 2.0 {
		halfLifeMs = 2.0
	} else if halfLifeMs > 500.0 {
		halfLifeMs = 500.0
	}

	return decay5msBps, decay25msBps, decay100msBps, halfLifeMs
}
