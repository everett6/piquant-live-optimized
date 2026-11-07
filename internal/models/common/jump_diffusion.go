package common

import (
	"math"
)

// JumpWindowSize is the fixed rolling window size for jump-diffusion decomposition.
const JumpWindowSize = 50

// JumpState contains the decomposed variance, volatility, and jump activity metrics.
type JumpState struct {
	RealizedVar      float64 `json:"realized_var"`
	ContinuousVar    float64 `json:"continuous_var"`
	JumpVar          float64 `json:"jump_var"`
	ContinuousVolBps float64 `json:"continuous_vol_bps"`
	JumpVolBps       float64 `json:"jump_vol_bps"`
	JumpRatio        float64 `json:"jump_ratio"`
	IsJumpActive     bool    `json:"is_jump_active"`
	VolMultiplier    float64 `json:"vol_multiplier"`
}

// JumpDiffusionEstimator decomposes high-frequency asset returns into continuous
// diffusion and discontinuous Merton jump components via Bipower Variation (BV).
//
// Realized Variance:             RV = sum(r_i^2)
// Continuous Bipower Variation:  BV = (pi / 2) * sum(|r_i| * |r_{i-1}|)
// Jump Variation:                JV = max(0, RV - BV)
// Jump Contribution Ratio:       JR = JV / RV
//
// Operates on a fixed in-memory ring buffer with zero heap allocations (0 B/op, 0 allocs/op).
type JumpDiffusionEstimator struct {
	window        [JumpWindowSize]float64
	head          int
	count         int
	lastPrice     float64
	hasPrice      bool
	JumpThreshold float64 // Jump ratio threshold (default 0.40)
}

// NewJumpDiffusionEstimator creates an instance of JumpDiffusionEstimator.
func NewJumpDiffusionEstimator() *JumpDiffusionEstimator {
	return &JumpDiffusionEstimator{
		JumpThreshold: 0.40,
	}
}

// Reset clears the historical price and return state.
func (e *JumpDiffusionEstimator) Reset() {
	e.head = 0
	e.count = 0
	e.lastPrice = 0
	e.hasPrice = false
}

// AddPrice updates the estimator with a new price print, calculates fractional return,
// and returns the latest JumpState in 0 B/op and 0 allocs/op.
func (e *JumpDiffusionEstimator) AddPrice(price float64) JumpState {
	if price <= 0 {
		return e.Compute()
	}
	if !e.hasPrice {
		e.lastPrice = price
		e.hasPrice = true
		return JumpState{VolMultiplier: 1.0}
	}
	r := (price - e.lastPrice) / e.lastPrice
	e.lastPrice = price
	return e.AddReturn(r)
}

// AddReturn appends a single fractional return into the fixed rolling buffer
// and recomputes the decomposition in 0 B/op and 0 allocs/op.
func (e *JumpDiffusionEstimator) AddReturn(r float64) JumpState {
	e.window[e.head] = r
	e.head = (e.head + 1) % JumpWindowSize
	if e.count < JumpWindowSize {
		e.count++
	}
	return e.Compute()
}

// Compute calculates Realized Variance (RV), Continuous Bipower Variation (BV),
// Jump Variation (JV = max(0, RV - BV)), and Jump Ratio (JR = JV / RV).
// Executes in 0 B/op and 0 allocs/op.
func (e *JumpDiffusionEstimator) Compute() JumpState {
	if e.count < 2 {
		return JumpState{VolMultiplier: 1.0}
	}

	n := e.count
	start := (e.head - n + JumpWindowSize) % JumpWindowSize

	var rv, rawBv float64
	for i := 0; i < n; i++ {
		idx := (start + i) % JumpWindowSize
		r := e.window[idx]
		rv += r * r
		if i > 0 {
			prevIdx := (start + i - 1) % JumpWindowSize
			prevR := e.window[prevIdx]
			rawBv += math.Abs(r) * math.Abs(prevR)
		}
	}

	bv := (math.Pi / 2.0) * rawBv
	jv := rv - bv
	if jv < 0 {
		jv = 0
	}

	jr := 0.0
	if rv > 1e-18 {
		jr = jv / rv
		if jr > 1.0 {
			jr = 1.0
		}
	}

	thresh := e.JumpThreshold
	if thresh <= 0 {
		thresh = 0.40
	}

	isJumpActive := jr > thresh

	volMultiplier := 1.0
	if isJumpActive {
		// Spread expansion scales with jump severity: [1.0 -> 3.0x]
		volMultiplier = 1.0 + (jr-thresh)*3.3333333333333335
		if volMultiplier > 3.0 {
			volMultiplier = 3.0
		}
	}

	contVolBps := math.Sqrt(math.Max(0, bv)) * 10000.0
	jumpVolBps := math.Sqrt(jv) * 10000.0

	return JumpState{
		RealizedVar:      rv,
		ContinuousVar:    bv,
		JumpVar:          jv,
		ContinuousVolBps: contVolBps,
		JumpVolBps:       jumpVolBps,
		JumpRatio:        jr,
		IsJumpActive:     isJumpActive,
		VolMultiplier:    volMultiplier,
	}
}
