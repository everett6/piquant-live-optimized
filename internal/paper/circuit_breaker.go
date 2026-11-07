package paper

import (
	"math"
	"time"
)

// RiskState represents the autonomous intraday trading risk state.
type RiskState int

const (
	StateNormal RiskState = iota
	StateWarning
	StateReduceOnly
	StateTripped
)

func (s RiskState) String() string {
	switch s {
	case StateNormal:
		return "NORMAL"
	case StateWarning:
		return "WARNING"
	case StateReduceOnly:
		return "REDUCE_ONLY"
	case StateTripped:
		return "TRIPPED"
	default:
		return "NORMAL"
	}
}

// SizingMultiplier returns the fraction of standard order sizing permitted in current state.
func (s RiskState) SizingMultiplier() float64 {
	switch s {
	case StateNormal:
		return 1.0 // 100% sizing
	case StateWarning:
		return 0.5 // 50% sizing
	case StateReduceOnly:
		return 0.0 // Close positions only
	case StateTripped:
		return 0.0 // Zero sizing
	default:
		return 1.0
	}
}

// CanEnterNewPositions reports whether open orders or new risk positions can be initiated.
func (s RiskState) CanEnterNewPositions() bool {
	return s == StateNormal || s == StateWarning
}

// CanReduceOnly reports whether risk-reducing orders are permitted.
func (s RiskState) CanReduceOnly() bool {
	return s != StateTripped
}

// Pre-allocated static reason constants for zero-allocation state reporting.
const (
	ReasonNormal               = "NORMAL"
	ReasonDrawdownWarning      = "DRAWDOWN_WARNING"
	ReasonDrawdownTripped      = "MAX_DRAWDOWN_EXCEEDED"
	ReasonInventoryCapExceeded = "INVENTORY_CAP_EXCEEDED"
	ReasonConsecutiveLosses    = "CONSECUTIVE_LOSS_LIMIT"
	ReasonSpreadBlowout        = "SPREAD_BLOWOUT"
	ReasonVPINToxic            = "VPIN_TOXICITY"
	ReasonInCooldown           = "IN_COOLDOWN"
	ReasonCooldownRecovered    = "COOLDOWN_RECOVERED"
)

// CircuitBreakerConfig defines institutional thresholds for intraday autonomous risk safeguards.
type CircuitBreakerConfig struct {
	WarningDrawdownPct       float64       // Warning threshold drawdown % (default: 2.5%)
	TrippedDrawdownPct       float64       // Tripped threshold drawdown % (default: 3.5%)
	MaxInventory             float64       // Inventory cap |q| (default: 2.0 BTC)
	WarningConsecutiveLosses int           // Consecutive fill losses triggering warning (default: 5)
	MaxSpreadBps             float64       // Spread blowout threshold (default: 50.0 bps)
	VPINThreshold            float64       // Toxic flow cutoff (default: 0.70)
	CooldownDuration         time.Duration // Required stable pricing duration before un-tripping (default: 5 min)
}

// DefaultCircuitBreakerConfig returns institutional default risk safeguards.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		WarningDrawdownPct:       2.5,
		TrippedDrawdownPct:       3.5,
		MaxInventory:             2.0,
		WarningConsecutiveLosses: 5,
		MaxSpreadBps:             50.0,
		VPINThreshold:            0.70,
		CooldownDuration:         5 * time.Minute,
	}
}

// CircuitBreaker implements the real-time intraday risk state machine for autonomous trading.
//
// All methods execute on the hot path in 0 B/op and 0 allocs/op.
type CircuitBreaker struct {
	cfg CircuitBreakerConfig

	// Intraday equity & drawdown tracking
	PeakEquity         float64
	CurrentEquity      float64
	CurrentDrawdownPct float64
	DrawdownBps        float64

	// Position & trade metrics
	CurrentInventory  float64
	ConsecutiveLosses int

	// Latest market conditions
	LastSpreadBps float64
	LastVPIN      float64

	// State machine
	State       RiskState
	TripReason  string
	TrippedAt   time.Time
	CooldownEnd time.Time
}

// NewCircuitBreaker initializes the risk circuit breaker engine.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.WarningDrawdownPct <= 0 {
		cfg.WarningDrawdownPct = 2.5
	}
	if cfg.TrippedDrawdownPct <= 0 {
		cfg.TrippedDrawdownPct = 3.5
	}
	if cfg.MaxInventory <= 0 {
		cfg.MaxInventory = 2.0
	}
	if cfg.WarningConsecutiveLosses <= 0 {
		cfg.WarningConsecutiveLosses = 5
	}
	if cfg.MaxSpreadBps <= 0 {
		cfg.MaxSpreadBps = 50.0
	}
	if cfg.VPINThreshold <= 0 {
		cfg.VPINThreshold = 0.70
	}
	if cfg.CooldownDuration <= 0 {
		cfg.CooldownDuration = 5 * time.Minute
	}
	return &CircuitBreaker{
		cfg:        cfg,
		State:      StateNormal,
		TripReason: ReasonNormal,
	}
}

// OnFill updates fill statistics (realized PnL, new inventory) and checks consecutive loss / inventory caps.
// Executes on the hot path in 0 B/op, 0 allocs/op.
func (cb *CircuitBreaker) OnFill(realizedPnL, newInventory float64) (RiskState, string) {
	cb.CurrentInventory = newInventory
	if realizedPnL < 0 {
		cb.ConsecutiveLosses++
	} else if realizedPnL > 0 {
		cb.ConsecutiveLosses = 0
	}

	if cb.State == StateTripped {
		return cb.State, cb.TripReason
	}

	// Inventory cap check
	if math.Abs(newInventory) > cb.cfg.MaxInventory {
		cb.State = StateReduceOnly
		cb.TripReason = ReasonInventoryCapExceeded
		return cb.State, cb.TripReason
	}

	// Consecutive losses check
	if cb.ConsecutiveLosses >= cb.cfg.WarningConsecutiveLosses {
		if cb.State == StateNormal {
			cb.State = StateWarning
			cb.TripReason = ReasonConsecutiveLosses
		}
		return cb.State, cb.TripReason
	}

	return cb.State, cb.TripReason
}

// OnTick processes live market updates (current equity, spread, VPIN toxicity) and enforces risk state transitions.
// Executes on the hot path in 0 B/op, 0 allocs/op.
func (cb *CircuitBreaker) OnTick(equity, spreadBps, vpin float64, now time.Time) (RiskState, string) {
	cb.CurrentEquity = equity
	if equity > cb.PeakEquity {
		cb.PeakEquity = equity
	}
	if cb.PeakEquity > 0 {
		dd := (cb.PeakEquity - equity) / cb.PeakEquity * 100.0
		if dd < 0 {
			dd = 0
		}
		cb.CurrentDrawdownPct = dd
		cb.DrawdownBps = dd * 100.0
	}
	cb.LastSpreadBps = spreadBps
	cb.LastVPIN = vpin

	// If currently Tripped, evaluate cooldown logic
	if cb.State == StateTripped {
		// Pricing stability requirement: spread within limits, VPIN non-toxic, and drawdown below tripped threshold
		isPricingStable := spreadBps <= cb.cfg.MaxSpreadBps && vpin < cb.cfg.VPINThreshold && cb.CurrentDrawdownPct < cb.cfg.TrippedDrawdownPct
		if !isPricingStable {
			// Reset cooldown if market conditions remain unstable
			cb.CooldownEnd = now.Add(cb.cfg.CooldownDuration)
			cb.TripReason = ReasonInCooldown
			return cb.State, cb.TripReason
		}

		if now.After(cb.CooldownEnd) {
			// Cooldown period completed with stable pricing: transition out of TRIPPED
			if math.Abs(cb.CurrentInventory) > cb.cfg.MaxInventory {
				cb.State = StateReduceOnly
				cb.TripReason = ReasonInventoryCapExceeded
			} else if cb.CurrentDrawdownPct >= cb.cfg.WarningDrawdownPct || cb.ConsecutiveLosses >= cb.cfg.WarningConsecutiveLosses {
				cb.State = StateWarning
				cb.TripReason = ReasonDrawdownWarning
			} else {
				cb.State = StateNormal
				cb.TripReason = ReasonCooldownRecovered
			}
		}
		return cb.State, cb.TripReason
	}

	// Check Trip conditions (cancel all quotes, zero sizing)
	if cb.CurrentDrawdownPct >= cb.cfg.TrippedDrawdownPct {
		cb.State = StateTripped
		cb.TripReason = ReasonDrawdownTripped
		cb.TrippedAt = now
		cb.CooldownEnd = now.Add(cb.cfg.CooldownDuration)
		return cb.State, cb.TripReason
	}

	if spreadBps > cb.cfg.MaxSpreadBps {
		cb.State = StateTripped
		cb.TripReason = ReasonSpreadBlowout
		cb.TrippedAt = now
		cb.CooldownEnd = now.Add(cb.cfg.CooldownDuration)
		return cb.State, cb.TripReason
	}

	// Check Reduce-Only conditions (close positions only)
	if math.Abs(cb.CurrentInventory) > cb.cfg.MaxInventory {
		cb.State = StateReduceOnly
		cb.TripReason = ReasonInventoryCapExceeded
		return cb.State, cb.TripReason
	}

	if vpin >= cb.cfg.VPINThreshold {
		cb.State = StateReduceOnly
		cb.TripReason = ReasonVPINToxic
		return cb.State, cb.TripReason
	}

	// Check Warning conditions (50% sizing)
	if cb.CurrentDrawdownPct >= cb.cfg.WarningDrawdownPct {
		cb.State = StateWarning
		cb.TripReason = ReasonDrawdownWarning
		return cb.State, cb.TripReason
	}

	if cb.ConsecutiveLosses >= cb.cfg.WarningConsecutiveLosses {
		cb.State = StateWarning
		cb.TripReason = ReasonConsecutiveLosses
		return cb.State, cb.TripReason
	}

	// Normal operation
	cb.State = StateNormal
	cb.TripReason = ReasonNormal
	return cb.State, cb.TripReason
}

// Reset clears the circuit breaker state and re-anchors peak equity.
func (cb *CircuitBreaker) Reset(initialEquity float64) {
	cb.PeakEquity = initialEquity
	cb.CurrentEquity = initialEquity
	cb.CurrentDrawdownPct = 0
	cb.DrawdownBps = 0
	cb.CurrentInventory = 0
	cb.ConsecutiveLosses = 0
	cb.State = StateNormal
	cb.TripReason = ReasonNormal
	cb.TrippedAt = time.Time{}
	cb.CooldownEnd = time.Time{}
}
