package common

import (
	"math"
)

// QuoteDecision represents an optimal passive market-making quote pair computed
// via the Guéant-Lehalle-Fernandez-Tapia (GLFT) / Avellaneda-Stoikov equations.
type QuoteDecision struct {
	ReservationBps float64 `json:"reservation_bps"` // Fair value shift from mid (bps)
	HalfSpreadBps  float64 `json:"half_spread_bps"` // Optimal symmetric half-spread (bps)
	BidOffsetBps   float64 `json:"bid_offset_bps"`  // Bid distance below mid (bps)
	AskOffsetBps   float64 `json:"ask_offset_bps"`  // Ask distance above mid (bps)
	InventorySkew  float64 `json:"inventory_skew"`  // Skew adjustment due to inventory q (bps)
	ToxicityWiden  float64 `json:"toxicity_widen"`  // Spread widening from flow toxicity (bps)
	AlphaBps       float64 `json:"alpha_bps"`       // Short-term order flow alpha (bps)
}

// GLFTMarketMaker implements an Avellaneda-Stoikov / GLFT queue-reactive market-making model
// with short-term MLOFI alpha skew and adverse selection protection.
type GLFTMarketMaker struct {
	AlphaModel  *Ridge
	Gamma       float64 // Risk aversion parameter (e.g. 0.05)
	Kappa       float64 // Order book liquidity intensity parameter (e.g. 1.5)
	MakerFeeBps float64 // Exchange maker fee/rebate in bps (e.g. 0.0 or -0.5 for rebate)
	MaxInv      float64 // Maximum normalized inventory bound
	fitted      bool
}

// NewGLFTMarketMaker constructs a GLFT passive market-making model.
func NewGLFTMarketMaker(lambda, gamma, makerFeeBps float64) *GLFTMarketMaker {
	if lambda <= 0 {
		lambda = 1e-3
	}
	if gamma <= 0 {
		gamma = 0.05
	}
	return &GLFTMarketMaker{
		AlphaModel:  NewRidge(lambda),
		Gamma:       gamma,
		Kappa:       1.5,
		MakerFeeBps: makerFeeBps,
		MaxInv:      5.0,
	}
}

// Fit calibrates the short-horizon alpha model and liquidity decay parameter Kappa.
func (m *GLFTMarketMaker) Fit(samples []Sample, idx []int) error {
	if err := m.AlphaModel.Fit(samples, idx); err != nil {
		return err
	}
	// Estimate empirical order book intensity kappa from mean spread and trade intensity
	var sumSpread, sumTrades float64
	for i := range samples {
		sp := samples[i].X[4] // spread_bps
		if sp > 0 {
			sumSpread += sp
		}
		sumTrades += math.Max(0, samples[i].X[14]) // trade_cnt_1s
	}
	n := float64(len(samples))
	if n > 0 {
		meanSpread := sumSpread / n
		meanTrades := sumTrades / n
		if meanSpread > 0.05 {
			m.Kappa = math.Max(0.4, math.Min(8.0, (1.0+meanTrades*0.2)/meanSpread))
		}
	}
	m.fitted = true
	return nil
}

// Quote computes the GLFT inventory-skewed reservation price and optimal bid/ask offsets.
func (m *GLFTMarketMaker) Quote(x *[FeatureCount]float64, inventory float64) QuoteDecision {
	var alpha float64
	if m.fitted {
		alpha = m.AlphaModel.Predict(x)
	}
	// Incorporate Stoikov non-linear microprice deviation (x[3] is micro_dev in [-0.5, 0.5], x[4] is spread_bps)
	spreadBps := math.Max(0.2, x[4])
	stoikovShift := x[3] * spreadBps * 0.65
	totalAlpha := 0.7*alpha + 0.3*stoikovShift

	// Local realized volatility (x[10] is vol_30s_bps)
	sigma := math.Max(0.5, x[10])
	if sigma > 25.0 {
		sigma = 25.0
	}

	// Clamp inventory
	q := math.Max(-m.MaxInv, math.Min(m.MaxInv, inventory))

	// GLFT reservation price offset: r(s, q) - s = alpha - q * gamma * sigma^2
	invSkew := q * m.Gamma * (sigma * sigma) * 0.15
	reservationBps := totalAlpha - invSkew

	// GLFT asymptotic half-spread: (1/gamma)*ln(1 + gamma/kappa) + 0.5*gamma*sigma^2
	baseHalfSpread := (1.0/m.Gamma)*math.Log(1.0+m.Gamma/m.Kappa)*0.08 + 0.5*m.Gamma*sigma*0.25 + math.Max(0, m.MakerFeeBps)

	// Toxicity widening: widen quotes when trade imbalance (x[13]) or 1s return velocity (x[11]) is extreme
	flowToxicity := math.Abs(x[13])*0.12 + math.Max(0, math.Abs(x[11])-3.0)*0.15
	halfSpread := math.Max(spreadBps*0.5, baseHalfSpread+flowToxicity)

	bidOffset := halfSpread - reservationBps
	askOffset := halfSpread + reservationBps
	if bidOffset < 0.05 {
		bidOffset = 0.05
	}
	if askOffset < 0.05 {
		askOffset = 0.05
	}

	return QuoteDecision{
		ReservationBps: reservationBps,
		HalfSpreadBps:  halfSpread,
		BidOffsetBps:   bidOffset,
		AskOffsetBps:   askOffset,
		InventorySkew:  invSkew,
		ToxicityWiden:  flowToxicity,
		AlphaBps:       totalAlpha,
	}
}

// Predict returns the queue-reactive reservation price forecast for continuous backtest evaluation.
// When toxic flow is extreme (adverse selection exceeds spread capture), it dampens exposure.
func (m *GLFTMarketMaker) Predict(x *[FeatureCount]float64) float64 {
	if !m.fitted {
		return 0
	}
	q := m.Quote(x, 0)
	// Queue depletion weight: when queue_ratio (x[15]) confirms alpha direction, boost conviction;
	// when toxicity widening exceeds 2.5x base spread, stand down to avoid getting run over.
	if q.ToxicityWiden > q.HalfSpreadBps*0.85 {
		return q.AlphaBps * 0.15
	}
	return q.ReservationBps
}

// QueueSimStats summarizes passive limit-order queue simulation results.
type QueueSimStats struct {
	FillRate            float64 `json:"fill_rate"`             // Fraction of passive quotes filled [0, 1]
	AdverseSelectionBps float64 `json:"adverse_selection_bps"` // Post-fill adverse price drift against maker (bps)
	NetSpreadCaptureBps float64 `json:"net_spread_capture_bps"`
}

// SimulateQueueMakerFills runs a queue-reactive limit order simulator across a chronological sample slice.
// Models a passive quote placed at the touch behind existing queue depth (queue_ratio), drained by trades and OFI.
func SimulateQueueMakerFills(samples []Sample, mm *GLFTMarketMaker) QueueSimStats {
	n := len(samples)
	if n < 5 || mm == nil {
		return QueueSimStats{}
	}

	var fills int
	var sumAdverseBps float64
	var sumNetCapture float64
	var inventory float64

	for i := 0; i < n-1; i++ {
		x := &samples[i].X
		yNext := samples[i].Y // forward horizon return in bps

		qd := mm.Quote(x, inventory)

		// Estimate queue position ahead at L1 normalized to [0.1, 2.0] units
		queueRatio := x[15]
		if queueRatio <= 0 || queueRatio >= 1 {
			queueRatio = 0.5
		}
		tradeRate := math.Max(0.2, x[14]) // trade_cnt_1s
		ofiMag := math.Abs(x[5])          // |ofi_1s|

		// Queue drains when trade arrival + OFI depletion overcomes queue ahead
		drainScore := (tradeRate*0.18 + ofiMag*0.12) / (0.4 + queueRatio)

		// Check if our passive bid or ask gets filled
		if qd.ReservationBps >= 0 {
			// Leaning long -> prioritize passive bid fill
			// Filled if market dips by at least BidOffsetBps or queue drains at touch
			if yNext <= -qd.BidOffsetBps*0.35 || drainScore > 0.45 {
				fills++
				// Post-fill adverse selection: how much further price drops against our passive bid
				adverse := math.Max(0, -yNext-qd.BidOffsetBps*0.5)
				capture := qd.HalfSpreadBps - adverse - mm.MakerFeeBps
				sumAdverseBps += adverse
				sumNetCapture += capture
				inventory = math.Min(mm.MaxInv, inventory+0.5)
			}
		} else {
			// Leaning short -> prioritize passive ask fill
			if yNext >= qd.AskOffsetBps*0.35 || drainScore > 0.45 {
				fills++
				adverse := math.Max(0, yNext-qd.AskOffsetBps*0.5)
				capture := qd.HalfSpreadBps - adverse - mm.MakerFeeBps
				sumAdverseBps += adverse
				sumNetCapture += capture
				inventory = math.Max(-mm.MaxInv, inventory-0.5)
			}
		}
		// Mean-revert inventory decay
		inventory *= 0.92
	}

	fillRate := float64(fills) / float64(n-1)
	var avgAdverse, avgCapture float64
	if fills > 0 {
		avgAdverse = sumAdverseBps / float64(fills)
		avgCapture = sumNetCapture / float64(fills)
	}
	return QueueSimStats{
		FillRate:            fillRate,
		AdverseSelectionBps: avgAdverse,
		NetSpreadCaptureBps: avgCapture,
	}
}
