package paper

import (
	"fmt"
	"math"
	"strings"
)

// DynamicLeverageConfig defines risk parameters for automated leverage scaling.
type DynamicLeverageConfig struct {
	MinLeverage    float64 `json:"min_leverage"`     // Minimum allowable leverage (default: 0.5x, 0.0x on halt)
	MaxLeverage    float64 `json:"max_leverage"`     // Maximum leverage ceiling (default: 5.0x for $1k capital)
	BaseLeverage   float64 `json:"base_leverage"`    // Nominal leverage under calm market making (default: 2.5x)
	TargetVolBps   float64 `json:"target_vol_bps"`   // Target tick volatility in basis points (default: 15.0 bps)
	MaxDrawdownPct float64 `json:"max_drawdown_pct"` // Circuit breaker drawdown threshold (default: 2.0%)
}

// DefaultDynamicLeverageConfig returns standard quantitative risk parameters.
func DefaultDynamicLeverageConfig() DynamicLeverageConfig {
	return DynamicLeverageConfig{
		MinLeverage:    0.5,
		MaxLeverage:    5.0,
		BaseLeverage:   2.5,
		TargetVolBps:   15.0,
		MaxDrawdownPct: 2.0,
	}
}

// DynamicLeverageState reports live leverage diagnostics for UI and telemetry.
type DynamicLeverageState struct {
	EffectiveLeverage float64 `json:"effective_leverage"` // Current active leverage multiplier (e.g. 2.45x)
	TargetLeverage    float64 `json:"target_leverage"`    // Raw unconstrained model target leverage
	MaxLeverage       float64 `json:"max_leverage"`       // Upper leverage cap (5.0x)
	MinLeverage       float64 `json:"min_leverage"`       // Lower leverage floor (0.5x)
	VolFactor         float64 `json:"vol_factor"`         // Volatility scaling factor (0.4 - 1.6)
	ToxicityFactor    float64 `json:"toxicity_factor"`    // VPIN toxicity dampener (0.2 - 1.0)
	CascadeFactor     float64 `json:"cascade_factor"`     // Hawkes cascade dampener (0.2 - 1.0)
	DrawdownFactor    float64 `json:"drawdown_factor"`    // Drawdown taper factor (0.0 - 1.0)
	RegimeName        string  `json:"regime_name"`        // e.g. "CALM_MAKER", "VOL_SCALED", "TOXIC_DEFENSE"
	RegimeDetail      string  `json:"regime_detail"`      // Explanatory breakdown string
}

// DynamicLeverageController automatically computes optimal leverage on every tick.
type DynamicLeverageController struct {
	cfg DynamicLeverageConfig
}

// NewDynamicLeverageController constructs an automated leverage controller.
func NewDynamicLeverageController(cfg DynamicLeverageConfig) *DynamicLeverageController {
	if cfg.MinLeverage <= 0 {
		cfg.MinLeverage = 0.5
	}
	if cfg.MaxLeverage <= 0 {
		cfg.MaxLeverage = 5.0
	}
	if cfg.BaseLeverage <= 0 {
		cfg.BaseLeverage = 2.5
	}
	if cfg.TargetVolBps <= 0 {
		cfg.TargetVolBps = 15.0
	}
	if cfg.MaxDrawdownPct <= 0 {
		cfg.MaxDrawdownPct = 2.0
	}
	return &DynamicLeverageController{cfg: cfg}
}

// Compute evaluates real-time market microstructure and computes optimal leverage and notional sizing.
func (c *DynamicLeverageController) Compute(volBps float64, vpin float64, hawkesBranching float64, currentDDPct float64, isCircuitHalted bool) DynamicLeverageState {
	if isCircuitHalted {
		return DynamicLeverageState{
			EffectiveLeverage: 0.0,
			TargetLeverage:    0.0,
			MaxLeverage:       c.cfg.MaxLeverage,
			MinLeverage:       c.cfg.MinLeverage,
			DrawdownFactor:    0.0,
			RegimeName:        "HALTED",
			RegimeDetail:      "Circuit breaker tripped: zero leverage / emergency halt",
		}
	}

	// 1. Volatility Scaling (Inverse volatility parity)
	vol := math.Max(1.0, volBps)
	volFactor := c.cfg.TargetVolBps / vol
	if volFactor > 1.6 {
		volFactor = 1.6
	} else if volFactor < 0.4 {
		volFactor = 0.4
	}

	// 2. VPIN Toxicity De-leveraging
	toxFct := 1.0
	if vpin > 0.55 {
		toxFct = math.Max(0.20, 0.70-2.0*(vpin-0.55))
	} else if vpin > 0.35 {
		toxFct = 1.0 - 1.5*(vpin-0.35)
	}

	// 3. Hawkes Cascade Contagion Factor
	cascFct := 1.0
	if hawkesBranching >= 0.90 {
		cascFct = 0.25
	} else if hawkesBranching >= 0.70 {
		cascFct = 0.65
	}

	// 4. Drawdown Preservation Taper
	ddFct := 1.0
	if c.cfg.MaxDrawdownPct > 0 {
		ddRatio := currentDDPct / c.cfg.MaxDrawdownPct
		if ddRatio >= 1.0 {
			ddFct = 0.0
		} else {
			ddFct = math.Max(0.10, 1.0-ddRatio)
		}
	}

	// 5. Synthesize target & constrained effective leverage
	targetLev := c.cfg.BaseLeverage * volFactor * toxFct * cascFct * ddFct
	effectiveLev := targetLev
	if effectiveLev > c.cfg.MaxLeverage {
		effectiveLev = c.cfg.MaxLeverage
	} else if effectiveLev < c.cfg.MinLeverage {
		effectiveLev = c.cfg.MinLeverage
	}

	// Determine descriptive regime
	regime := "CALM_MAKER"
	detail := fmt.Sprintf("Optimal spread capture (%.1fx Kelly vol-parity)", effectiveLev)

	if cascFct < 0.5 {
		regime = "CASCADE_ALERT"
		detail = fmt.Sprintf("Hawkes cascade spike (branching %.2f): leverage cut to %.1fx", hawkesBranching, effectiveLev)
	} else if toxFct < 0.6 {
		regime = "TOXIC_DEFENSE"
		detail = fmt.Sprintf("Informed toxic flow (VPIN %.2f): leverage dialed down to %.1fx", vpin, effectiveLev)
	} else if volFactor < 0.7 {
		regime = "HIGH_VOLATILITY"
		detail = fmt.Sprintf("Elevated volatility (%.1f bps): leverage scaled to %.1fx", volBps, effectiveLev)
	} else if ddFct < 0.6 {
		regime = "DRAWDOWN_TAPER"
		detail = fmt.Sprintf("Capital preservation taper (DD: %.2f%%): leverage reduced to %.1fx", currentDDPct, effectiveLev)
	}

	return DynamicLeverageState{
		EffectiveLeverage: effectiveLev,
		TargetLeverage:    targetLev,
		MaxLeverage:       c.cfg.MaxLeverage,
		MinLeverage:       c.cfg.MinLeverage,
		VolFactor:         volFactor,
		ToxicityFactor:    toxFct,
		CascadeFactor:     cascFct,
		DrawdownFactor:    ddFct,
		RegimeName:        regime,
		RegimeDetail:      detail,
	}
}

// ComputeOrderSize computes the quote size for a given asset given current portfolio equity and price.
// Implements cross-asset volatility parity (ETH receives proportional vol-scaled allocation).
func (c *DynamicLeverageController) ComputeOrderSize(equity float64, effectiveLev float64, price float64, numSymbols int) float64 {
	return c.ComputeOrderSizeForSymbol("BTCUSDT", equity, effectiveLev, price, numSymbols)
}

// ComputeOrderSizeForSymbol scales sizing per instrument based on volatility parity:
// ETHUSDT (higher vol) has smaller fractional size to equate dollar volatility risk,
// while preserving wider spread capture.
func (c *DynamicLeverageController) ComputeOrderSizeForSymbol(symbol string, equity float64, effectiveLev float64, price float64, numSymbols int) float64 {
	if equity <= 0 || price <= 0 || effectiveLev <= 0 {
		return 0.001
	}
	if numSymbols <= 0 {
		numSymbols = 2
	}
	// Allocate notional across traded universe
	baseNotional := (equity * effectiveLev) / float64(numSymbols)

	// Volatility parity weighting: ETH volatility (~70%) vs BTC volatility (~45%)
	// Vol weight = TargetVol / AssetVol
	weight := 1.0
	if strings.Contains(strings.ToUpper(symbol), "ETH") {
		weight = 0.85 // Vol-parity dampener so ETH dollar variance matches BTC
	} else if strings.Contains(strings.ToUpper(symbol), "BTC") {
		weight = 1.15
	}

	notional := baseNotional * weight
	qty := notional / price

	if strings.Contains(strings.ToUpper(symbol), "ETH") {
		return math.Round(qty*1000) / 1000 // 3 decimals for ETH
	}
	// 4 decimals for BTC
	return math.Round(qty*10000) / 10000
}
