package common

import (
	"math"
)

// TriangularArbDecision contains the cross-rate synthetic pricing metrics,
// divergence, net edge net of fees, and triangular arbitrage leg execution sides.
type TriangularArbDecision struct {
	SyntheticPrice float64 `json:"synthetic_price"`
	DirectPrice    float64 `json:"direct_price"`
	DivergenceBps  float64 `json:"divergence_bps"`
	NetEdgeBps     float64 `json:"net_edge_bps"`
	ArbOpportunity bool    `json:"arb_opportunity"`
	Leg1Side       string  `json:"leg1_side"` // Leg A side: "BUY", "SELL", or "HOLD"
	Leg2Side       string  `json:"leg2_side"` // Leg B side: "BUY", "SELL", or "HOLD"
	Leg3Side       string  `json:"leg3_side"` // Leg C side: "BUY", "SELL", or "HOLD"
}

// TriangularArbModel models the triangular cross-rate relationship between three
// cointegrated cryptocurrency pairs:
//
//	Leg A: Base 1 / Quote 1 (e.g. BTCUSDT)
//	Leg B: Base 2 / Quote 1 (e.g. ETHUSDT)
//	Leg C: Base 2 / Base 1  (e.g. ETHBTC)
//
// Synthetic cross rate: P_synth = P_B / P_A
// Direct cross rate:    P_direct = P_C
// Divergence:           Delta_bps = ((P_C - P_synth) / P_synth) * 10,000
// Net edge:             NetEdge_bps = |Delta_bps| - (Fee_A + Fee_B + Fee_C)
//
// All update, evaluation, and prediction routines execute with zero heap allocations (0 B/op, 0 allocs/op).
type TriangularArbModel struct {
	LegASymbol string // e.g. "BTCUSDT"
	LegBSymbol string // e.g. "ETHUSDT"
	LegCSymbol string // e.g. "ETHBTC"

	FeeA         float64 // Fee in bps for Leg A (default 2.0 bps)
	FeeB         float64 // Fee in bps for Leg B (default 2.0 bps)
	FeeC         float64 // Fee in bps for Leg C (default 2.0 bps)
	ThresholdBps float64 // Minimum required net edge in bps to declare an opportunity

	PriceA float64
	PriceB float64
	PriceC float64

	// Baseline continuous predictor for experiment runner & feature scoring
	BaseRidge Ridge
	fitted    bool
	AlphaWt   float64
}

// NewTriangularArbModel creates an instance of TriangularArbModel.
func NewTriangularArbModel(lambda, feeA, feeB, feeC, thresholdBps float64) *TriangularArbModel {
	if feeA <= 0 {
		feeA = 2.0
	}
	if feeB <= 0 {
		feeB = 2.0
	}
	if feeC <= 0 {
		feeC = 2.0
	}
	if thresholdBps <= 0 {
		thresholdBps = 0.5
	}
	return &TriangularArbModel{
		LegASymbol:   "BTCUSDT",
		LegBSymbol:   "ETHUSDT",
		LegCSymbol:   "ETHBTC",
		FeeA:         feeA,
		FeeB:         feeB,
		FeeC:         feeC,
		ThresholdBps: thresholdBps,
		BaseRidge:    Ridge{Lam: lambda},
		AlphaWt:      0.50,
	}
}

// UpdatePrices updates the three leg prices and returns the latest TriangularArbDecision.
// Operates in 0 B/op and 0 allocs/op.
func (m *TriangularArbModel) UpdatePrices(priceA, priceB, priceC float64) TriangularArbDecision {
	m.PriceA = priceA
	m.PriceB = priceB
	m.PriceC = priceC
	return m.Evaluate()
}

// Evaluate computes the cross-rate synthetic price, divergence, and net arbitrage edge.
// Operates in 0 B/op and 0 allocs/op.
func (m *TriangularArbModel) Evaluate() TriangularArbDecision {
	if m.PriceA <= 0 || m.PriceB <= 0 || m.PriceC <= 0 {
		return TriangularArbDecision{
			Leg1Side: "HOLD",
			Leg2Side: "HOLD",
			Leg3Side: "HOLD",
		}
	}

	pSynth := m.PriceB / m.PriceA
	pDirect := m.PriceC
	divergenceBps := ((pDirect - pSynth) / pSynth) * 10000.0

	totalFeesBps := m.FeeA + m.FeeB + m.FeeC
	netEdgeBps := math.Abs(divergenceBps) - totalFeesBps

	arbOpp := netEdgeBps > m.ThresholdBps

	var leg1Side, leg2Side, leg3Side string
	if arbOpp {
		if divergenceBps > 0 {
			// Direct cross rate (ETHBTC) is overpriced compared to synthetic (ETHUSDT / BTCUSDT).
			// Sell direct leg C (Sell ETH for BTC), Sell BTC for USDT on Leg A, Buy ETH with USDT on Leg B.
			leg1Side = "SELL" // Leg A: BTCUSDT -> Sell BTC for USDT
			leg2Side = "BUY"  // Leg B: ETHUSDT -> Buy ETH with USDT
			leg3Side = "SELL" // Leg C: ETHBTC  -> Sell ETH for BTC
		} else {
			// Direct cross rate (ETHBTC) is underpriced compared to synthetic.
			// Buy direct leg C (Buy ETH with BTC), Sell ETH for USDT on Leg B, Buy BTC with USDT on Leg A.
			leg1Side = "BUY"  // Leg A: BTCUSDT -> Buy BTC with USDT
			leg2Side = "SELL" // Leg B: ETHUSDT -> Sell ETH for USDT
			leg3Side = "BUY"  // Leg C: ETHBTC  -> Buy ETH with BTC
		}
	} else {
		leg1Side = "HOLD"
		leg2Side = "HOLD"
		leg3Side = "HOLD"
	}

	return TriangularArbDecision{
		SyntheticPrice: pSynth,
		DirectPrice:    pDirect,
		DivergenceBps:  divergenceBps,
		NetEdgeBps:     netEdgeBps,
		ArbOpportunity: arbOpp,
		Leg1Side:       leg1Side,
		Leg2Side:       leg2Side,
		Leg3Side:       leg3Side,
	}
}

// Fit trains the underlying Ridge predictor and calibrates model parameters on historical samples.
func (m *TriangularArbModel) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	if len(idx) == 0 {
		idx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := m.BaseRidge.Fit(samples, idx); err != nil {
		return err
	}
	m.fitted = true
	return nil
}

// Predict calculates forward alpha forecast in bps from the 16-element core feature vector.
// Operates in 0 B/op and 0 allocs/op.
func (m *TriangularArbModel) Predict(x *[FeatureCount]float64) float64 {
	baseAlpha := 0.0
	if m.fitted {
		baseAlpha = m.BaseRidge.Predict(x)
	}

	spread := math.Max(0.1, x[4])
	microEdge := x[3] * spread * 1.2
	flowEdge := 0.25*x[6] + 0.15*x[7] + 0.20*x[9]
	synthEdge := microEdge + flowEdge

	return baseAlpha + m.AlphaWt*synthEdge
}

// Clone returns an independent deep copy of the TriangularArbModel.
func (m *TriangularArbModel) Clone() *TriangularArbModel {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}
