package common

import (
	"math"
)

// InventoryAgingPolicy implements dynamic inventory aging and alpha-decay liquidation
// designed for high-frequency market making under adverse selection.
//
// When inventory sits stale, holding risk grows non-linearly with holding duration tau:
//
//	U(tau, q) = sign(q) * lambda_age * (tau / tau_half)^gamma
//
// The policy skews reservation quotes and forces progressive spread narrowing on the
// closing side when inventory grows stale (>30s, >60s), preventing adverse selection overhang.
//
// All methods execute with 0 B/op and 0 allocs/op.
type InventoryAgingPolicy struct {
	LambdaAge     float64 // Base age penalty scale in bps (default 1.5 bps)
	TauHalf       float64 // Reference aging half-life horizon in seconds (default 30.0s)
	Gamma         float64 // Non-linear aging exponent (default 1.5)
	MaxUrgencyBps float64 // Maximum urgency penalty cap in bps (default 30.0 bps)

	// State tracking
	Inventory     float64 // Current net inventory position
	EntryTimeNs   int64   // Timestamp of initial inventory entry (nanoseconds)
	AvgEntryPrice float64 // Volume-weighted entry price
}

// NewInventoryAgingPolicy instantiates an inventory aging policy with institutional defaults.
func NewInventoryAgingPolicy(lambdaAge, tauHalf, gamma float64) *InventoryAgingPolicy {
	if lambdaAge <= 0 {
		lambdaAge = 1.5
	}
	if tauHalf <= 0 {
		tauHalf = 30.0
	}
	if gamma <= 0 {
		gamma = 1.5
	}
	return &InventoryAgingPolicy{
		LambdaAge:     lambdaAge,
		TauHalf:       tauHalf,
		Gamma:         gamma,
		MaxUrgencyBps: 30.0,
	}
}

// UrgencyPenalty computes the non-linear time-decay urgency penalty U(tau, q) in basis points.
// Returns signed penalty (positive for long inventory, negative for short inventory).
// Operates in 0 B/op and 0 allocs/op.
func (p *InventoryAgingPolicy) UrgencyPenalty(inventory float64, holdingDurationSec float64) float64 {
	if inventory == 0 || holdingDurationSec <= 0 {
		return 0.0
	}

	sign := 1.0
	if inventory < 0 {
		sign = -1.0
	}

	ratio := holdingDurationSec / p.TauHalf
	rawPenalty := p.LambdaAge * math.Pow(ratio, p.Gamma)
	if rawPenalty > p.MaxUrgencyBps {
		rawPenalty = p.MaxUrgencyBps
	}

	return sign * rawPenalty
}

// AdjustQuotes adjusts bid, ask, and reservation prices based on inventory size and holding duration.
//
// When inventory is long (q > 0), the reservation price is skewed downward, the ask quote
// is narrowed (discounted) to attract closing executions, and the bid quote is pushed lower.
// When inventory is short (q < 0), reservation price is skewed upward, bid quote is narrowed
// (bid up) to cover shorts, and ask quote is raised.
// Stale inventory (>30s, >60s) triggers progressive spread narrowing on the closing side.
//
// Returns: adjustedBid, adjustedAsk, adjustedReservation, urgencyBps (absolute penalty in bps).
// Operates in 0 B/op and 0 allocs/op.
func (p *InventoryAgingPolicy) AdjustQuotes(bidPrice, askPrice, reservationPrice, inventory float64, holdingDurationSec float64) (adjustedBid, adjustedAsk, adjustedReservation float64, urgencyBps float64) {
	if inventory == 0 || holdingDurationSec <= 0 || reservationPrice <= 0 {
		return bidPrice, askPrice, reservationPrice, 0.0
	}

	signedPenalty := p.UrgencyPenalty(inventory, holdingDurationSec)
	urgencyBps = math.Abs(signedPenalty)
	if urgencyBps == 0 {
		return bidPrice, askPrice, reservationPrice, 0.0
	}

	r := reservationPrice
	halfSpread := (askPrice - bidPrice) * 0.5
	if halfSpread <= 0 {
		halfSpread = r * 0.0001 // fallback 1 bps
	}

	// Dynamic closing-side skew factor scaling with aging tier:
	// Normal (<30s): standard linear quote skew
	// Stale (>30s): accelerated narrowing on closing side
	// Critical (>60s): aggressive liquidation towards reservation / mid
	narrowFactor := 1.0
	if holdingDurationSec > 60.0 {
		narrowFactor = 2.0
	} else if holdingDurationSec > 30.0 {
		narrowFactor = 1.4
	}

	shiftPrice := r * (urgencyBps / 10000.0)

	if inventory > 0 {
		// Long inventory: urgent to SELL (close)
		adjustedReservation = r - shiftPrice

		// Narrow ask (closing side)
		askDiscount := shiftPrice * narrowFactor
		adjustedAsk = askPrice - askDiscount

		// Bid widened lower to prevent accumulating more longs
		adjustedBid = bidPrice - shiftPrice*0.75

		// When stale (>60s), ensure ask touches reservation price
		if holdingDurationSec > 60.0 && adjustedAsk > adjustedReservation+halfSpread*0.25 {
			adjustedAsk = adjustedReservation + halfSpread*0.25
		}
	} else {
		// Short inventory: urgent to BUY (cover)
		adjustedReservation = r + shiftPrice

		// Narrow bid (closing side)
		bidPremium := shiftPrice * narrowFactor
		adjustedBid = bidPrice + bidPremium

		// Ask widened higher to prevent accumulating more shorts
		adjustedAsk = askPrice + shiftPrice*0.75

		// When stale (>60s), ensure bid touches reservation price
		if holdingDurationSec > 60.0 && adjustedBid < adjustedReservation-halfSpread*0.25 {
			adjustedBid = adjustedReservation - halfSpread*0.25
		}
	}

	// Safety sanity invariant: bid must remain strictly below ask
	if adjustedBid >= adjustedAsk {
		mid := (adjustedBid + adjustedAsk) * 0.5
		adjustedBid = mid - halfSpread*0.1
		adjustedAsk = mid + halfSpread*0.1
	}

	return adjustedBid, adjustedAsk, adjustedReservation, urgencyBps
}

// OnFill updates the position size, average entry price, and entry timestamp.
// For institutional aging accuracy, adding to an existing position preserves the original
// entry timestamp so aging clocks cannot be reset by small trickle orders.
// Operates in 0 B/op and 0 allocs/op.
func (p *InventoryAgingPolicy) OnFill(qty float64, price float64, timestampNs int64) {
	if qty == 0 {
		return
	}

	oldInv := p.Inventory
	newInv := oldInv + qty

	if oldInv == 0 {
		// Fresh position entry
		p.Inventory = newInv
		p.EntryTimeNs = timestampNs
		p.AvgEntryPrice = price
		return
	}

	// Crossing zero (flip from long to short or vice versa)
	if (oldInv > 0 && newInv < 0) || (oldInv < 0 && newInv > 0) {
		p.Inventory = newInv
		p.EntryTimeNs = timestampNs
		p.AvgEntryPrice = price
		return
	}

	// Completely flattened
	if newInv == 0 {
		p.Inventory = 0
		p.EntryTimeNs = 0
		p.AvgEntryPrice = 0
		return
	}

	// Adding in the same direction: maintain earliest entry time to track true inventory age
	if (oldInv > 0 && qty > 0) || (oldInv < 0 && qty < 0) {
		totalCost := oldInv*p.AvgEntryPrice + qty*price
		p.Inventory = newInv
		p.AvgEntryPrice = totalCost / newInv
		// Keep original p.EntryTimeNs intact
		return
	}

	// Partial reduction: position size shrinks, entry time and unit cost remain identical
	p.Inventory = newInv
}

// HoldingDurationSec returns the current inventory holding duration in seconds.
func (p *InventoryAgingPolicy) HoldingDurationSec(nowNs int64) float64 {
	if p.Inventory == 0 || p.EntryTimeNs == 0 || nowNs <= p.EntryTimeNs {
		return 0.0
	}
	return float64(nowNs-p.EntryTimeNs) / 1e9
}

// Reset clears the inventory tracker state.
func (p *InventoryAgingPolicy) Reset() {
	p.Inventory = 0
	p.EntryTimeNs = 0
	p.AvgEntryPrice = 0
}
