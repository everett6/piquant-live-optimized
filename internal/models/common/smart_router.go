package common

import "math"

// RouteAction classifies the execution strategy selected by the SmartOrderRouter.
type RouteAction string

const (
	RouteMakerSpot       RouteAction = "MakerSpot"
	RouteMakerPerp       RouteAction = "MakerPerp"
	RouteTakerSpot       RouteAction = "TakerSpot"
	RouteTakerPerp       RouteAction = "TakerPerp"
	RouteSplitMakerTaker RouteAction = "SplitMakerTaker"
)

// RoutingDecision encapsulates the stack-allocated multi-venue Smart Order Router (SOR)
// and Maker/Taker optimization result.
type RoutingDecision struct {
	Action              RouteAction `json:"action"`
	MakerFraction       float64     `json:"maker_fraction"`        // Optimal passive maker share theta in [0, 1]
	TakerFraction       float64     `json:"taker_fraction"`        // Immediate taker share 1 - theta in [0, 1]
	SpotAllocation      float64     `json:"spot_allocation"`       // Fraction routed to Spot venue [0, 1]
	PerpAllocation      float64     `json:"perp_allocation"`       // Fraction routed to Perpetual Futures venue [0, 1]
	FillProbability     float64     `json:"fill_probability"`      // Cont-de Larrard queue fill probability P(fill)
	ExpectedEdgeBps     float64     `json:"expected_edge_bps"`     // Net expected alpha edge after fees, rebates & impact (bps)
	ExpectedSlippageBps float64     `json:"expected_slippage_bps"` // Expected Kyle's lambda + taker fee slippage drag (bps)
	PrimaryVenue        string      `json:"primary_venue"`         // "BINANCE_SPOT" or "BINANCE_PERP"
}

// SmartOrderRouter optimizes child order placement across passive Maker vs. immediate Taker
// execution and across Spot vs. Perpetual Futures venues using Huang-Lehalle-Rosenbaum queue-reactive
// fill survival probability (arXiv:2501.08822), Kyle's lambda square-root market impact, and perpetual basis carry.
//
// All methods execute in 0 B/op and 0 allocs/op.
type SmartOrderRouter struct {
	SpotMakerFeeBps     float64            // Spot maker fee/rebate in bps (e.g. 0.0 or -0.5 rebate)
	SpotTakerFeeBps     float64            // Spot taker fee in bps (e.g. 4.0)
	PerpMakerFeeBps     float64            // Perp maker fee/rebate in bps (e.g. -0.2 rebate)
	PerpTakerFeeBps     float64            // Perp taker fee in bps (e.g. 2.5)
	KylesLambda         float64            // Square-root price impact coefficient (bps per sqrt(unit size))
	HalfSpreadBps       float64            // Baseline half-spread captured by passive maker orders (bps)
	CancelRate          float64            // Baseline cancellation intensity (units/sec)
	TradeRate           float64            // Baseline trade depletion intensity (units/sec)
	HorizonSec          float64            // Target execution decision horizon (seconds)
	AdverseSelectionBps float64            // Baseline post-fill maker adverse selection cost (bps)
	QueueModel          QueueReactiveModel // Online-estimated queue-reactive intensity model
}

// NewSmartOrderRouter returns a calibrated multi-venue SmartOrderRouter with institutional defaults.
func NewSmartOrderRouter() SmartOrderRouter {
	return SmartOrderRouter{
		SpotMakerFeeBps:     0.0,
		SpotTakerFeeBps:     4.0,
		PerpMakerFeeBps:     -0.2,
		PerpTakerFeeBps:     2.5,
		KylesLambda:         0.35,
		HalfSpreadBps:       0.60,
		CancelRate:          1.50,
		TradeRate:           3.20,
		HorizonSec:          5.0,
		AdverseSelectionBps: 0.25,
		QueueModel:          NewQueueReactiveModel(),
	}
}

// Route evaluates whether an order of size orderSize with forecast alphaBps, queueAhead,
// local volatility, and perpetual basis perpBasisBps should be routed as MakerSpot,
// MakerPerp, TakerSpot, TakerPerp, or SplitMakerTaker.
//
// Guaranteed 0 B/op and 0 allocs/op.
func (s SmartOrderRouter) Route(alphaBps, orderSize, queueAhead, volatility, perpBasisBps float64) RoutingDecision {
	absAlpha := math.Abs(alphaBps)
	if orderSize <= 0 {
		orderSize = 0.01
	}
	if queueAhead < 0 {
		queueAhead = 0
	}
	if volatility <= 0 {
		volatility = 1.0
	}

	horizon := s.HorizonSec
	if horizon <= 0 {
		horizon = 5.0
	}

	// 1. Huang-Lehalle-Rosenbaum queue-reactive intensity fill probability P(fill) within horizonSec.
	effQueue := queueAhead + 0.5*orderSize
	pFill := s.QueueModel.FillProbabilityFor(horizon, effQueue, 0.0)

	// 2. Venue Selection (Spot vs. Perpetual Futures) factoring in perp basis carry & fee structure.
	// If buying (alphaBps >= 0), a negative perpBasisBps means Perp trades at a discount (positive carry).
	// If selling (alphaBps < 0), a positive perpBasisBps means Perp trades at a premium (positive carry).
	dirSign := 1.0
	if alphaBps < 0 {
		dirSign = -1.0
	}
	perpCarryAdvantageBps := -dirSign * perpBasisBps * 0.45
	perpFeeAdvantageBps := 0.5 * ((s.SpotMakerFeeBps - s.PerpMakerFeeBps) + (s.SpotTakerFeeBps - s.PerpTakerFeeBps))
	netPerpPref := perpCarryAdvantageBps + perpFeeAdvantageBps

	// Smooth logistical allocation between Perp and Spot in [0, 1]
	perpAlloc := 1.0 / (1.0 + math.Exp(-1.2*netPerpPref))
	if perpAlloc < 0.05 {
		perpAlloc = 0.0
	} else if perpAlloc > 0.95 {
		perpAlloc = 1.0
	}
	spotAlloc := 1.0 - perpAlloc

	primaryVenue := "BINANCE_PERP"
	if spotAlloc > perpAlloc {
		primaryVenue = "BINANCE_SPOT"
	}

	// Blended venue fees based on Spot/Perp split
	makerFeeBps := spotAlloc*s.SpotMakerFeeBps + perpAlloc*s.PerpMakerFeeBps
	takerFeeBps := spotAlloc*s.SpotTakerFeeBps + perpAlloc*s.PerpTakerFeeBps

	// 3. Kyle's Lambda square-root market impact for immediate taker execution
	volMultiplier := 1.0 + 0.15*math.Max(0, volatility-1.0)
	kyleImpactBps := s.KylesLambda * math.Sqrt(orderSize) * volMultiplier
	takerDragBps := takerFeeBps + kyleImpactBps

	// 4. Maker vs. Taker Expected Value Optimization:
	// Passive Maker captures half-spread minus maker fee and post-fill adverse selection,
	// weighted by Cont-de Larrard fill probability P(fill).
	adverseBps := s.AdverseSelectionBps * (1.0 + 0.10*volatility)
	makerUnitEdge := absAlpha + s.HalfSpreadBps - makerFeeBps - adverseBps + perpAlloc*perpCarryAdvantageBps
	expectedMakerEdge := pFill * makerUnitEdge

	// Immediate Taker captures alpha immediately minus taker fee and Kyle's lambda slippage
	expectedTakerEdge := absAlpha - takerDragBps + perpAlloc*perpCarryAdvantageBps

	// Opportunity cost of missed passive fill: alphaBps * (1 - P(fill))
	unfilledAlphaLoss := absAlpha * (1.0 - pFill)

	// Optimal maker fraction theta in [0, 1]:
	// When opportunity cost of missing the move exceeds taker execution cost, shift weight to Taker.
	var theta float64
	if unfilledAlphaLoss > takerDragBps && expectedTakerEdge > 0 {
		excessUrgency := (unfilledAlphaLoss - takerDragBps) / math.Max(1.0, takerDragBps)
		theta = pFill / (1.0 + 1.8*excessUrgency)
	} else if absAlpha > takerDragBps && pFill < 0.80 {
		// Moderate urgency with congested queue ahead -> interior split
		theta = 0.25 + 0.65*pFill
	} else {
		// Passive maker dominates when alpha is modest or fill probability is high
		theta = 0.90 + 0.10*pFill
	}

	if theta < 0 {
		theta = 0
	} else if theta > 1 {
		theta = 1
	}

	// Classify discrete routing regime vs continuous SplitMakerTaker
	var action RouteAction
	if theta >= 0.85 {
		theta = 1.0
		if perpAlloc >= 0.5 {
			action = RouteMakerPerp
		} else {
			action = RouteMakerSpot
		}
	} else if theta <= 0.15 {
		theta = 0.0
		if perpAlloc >= 0.5 {
			action = RouteTakerPerp
		} else {
			action = RouteTakerSpot
		}
	} else {
		action = RouteSplitMakerTaker
	}

	takerFrac := 1.0 - theta
	blendedEdgeBps := theta*expectedMakerEdge + takerFrac*expectedTakerEdge
	blendedSlippageBps := takerFrac*takerDragBps + theta*math.Max(0, makerFeeBps+adverseBps-s.HalfSpreadBps)

	return RoutingDecision{
		Action:              action,
		MakerFraction:       theta,
		TakerFraction:       takerFrac,
		SpotAllocation:      spotAlloc,
		PerpAllocation:      perpAlloc,
		FillProbability:     pFill,
		ExpectedEdgeBps:     blendedEdgeBps,
		ExpectedSlippageBps: blendedSlippageBps,
		PrimaryVenue:        primaryVenue,
	}
}
