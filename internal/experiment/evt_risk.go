package experiment

import (
	"errors"
	"math"
	"sort"
)

// ErrInsufficientExceedances is returned when the number of tail exceedances is too small to fit GPD.
var ErrInsufficientExceedances = errors.New("evt: insufficient exceedances above threshold u")

// GPDParams holds the fitted Generalized Pareto Distribution parameters.
type GPDParams struct {
	Threshold float64 `json:"threshold"` // Threshold u
	Xi        float64 `json:"xi"`        // Shape parameter \xi (tail index)
	Beta      float64 `json:"beta"`      // Scale parameter \beta (> 0)
	Nu        int     `json:"nu"`        // Number of exceedances (losses > u)
	N         int     `json:"n"`         // Total sample count
}

// EVTRiskEngine calculates Extreme Value Theory (EVT) Peaks-Over-Threshold (POT)
// Value-at-Risk (VaR) and Expected Shortfall (ES).
//
// Fits a Generalized Pareto Distribution (GPD) (\xi, \beta) via the method of
// Probability-Weighted Moments (PWM) and Method-of-Moments fallback.
// All evaluation methods execute in 0 B/op and 0 allocs/op.
type EVTRiskEngine struct {
	Params GPDParams
	fitted bool
}

// NewEVTRiskEngine creates an unfitted EVTRiskEngine.
func NewEVTRiskEngine() *EVTRiskEngine {
	return &EVTRiskEngine{}
}

// FitPOT fits the Generalized Pareto Distribution to the upper tail of losses
// exceeding threshold u using Probability-Weighted Moments (PWM).
//
// losses: slice of historical loss observations (positive values represent losses).
// threshold u: the high threshold above which asymptotic GPD holds.
func (e *EVTRiskEngine) FitPOT(losses []float64, u float64) error {
	n := len(losses)
	if n < 10 {
		return ErrInsufficientExceedances
	}

	// Extract excesses y_i = x_i - u > 0
	var excesses []float64
	for _, l := range losses {
		if l > u {
			excesses = append(excesses, l-u)
		}
	}
	nu := len(excesses)
	if nu < 5 {
		return ErrInsufficientExceedances
	}

	sort.Float64s(excesses)

	// Probability-Weighted Moments (PWM) estimation:
	// a_0 = E[Y]
	// a_1 = E[Y (1 - F(Y))]
	// Sample estimators:
	// a0 = (1/k) \sum y_j
	// a1 = (1/k) \sum ((k - j)/(k - 1)) * y_j
	// For GPD:
	// a0 = beta / (1 - xi)
	// a1 = beta / (2 * (2 - xi))
	// R = a0 / a1
	// xi = (R - 4) / (R - 2)
	// beta = a0 * (1 - xi)
	var sumY float64
	for _, y := range excesses {
		sumY += y
	}
	a0 := sumY / float64(nu)

	var sumW float64
	kFloat := float64(nu)
	for j := 1; j <= nu; j++ {
		w := (kFloat - float64(j)) / (kFloat - 1.0)
		sumW += w * excesses[j-1]
	}
	a1 := sumW / kFloat

	var xi, beta float64
	if a1 > 1e-12 && a0 > 2.0*a1 {
		r := a0 / a1
		if math.Abs(r-2.0) > 1e-6 {
			xiEst := (r - 4.0) / (r - 2.0)
			// Constrain xi for finite mean (xi < 1.0, typical financial range [-0.5, 0.5])
			if xiEst > -0.8 && xiEst < 0.8 {
				xi = xiEst
				beta = a0 * (1.0 - xi)
			}
		}
	}

	// Fallback to Method of Moments (MOM) if PWM produced out-of-bound estimate
	if beta <= 1e-9 {
		var sumSq float64
		for _, y := range excesses {
			diff := y - a0
			sumSq += diff * diff
		}
		variance := sumSq / float64(nu-1)
		if variance > 1e-9 && a0*a0 < variance {
			xi = 0.5 * (1.0 - (a0*a0)/variance)
			beta = 0.5 * a0 * (1.0 + (a0*a0)/variance)
		} else {
			// Exponential tail fallback (\xi = 0)
			xi = 0.0
			beta = a0
		}
	}

	// Ensure xi < 1.0 for finite Expected Shortfall
	if xi >= 0.99 {
		xi = 0.99
	}
	if beta <= 1e-9 {
		beta = 1e-4
	}

	e.Params = GPDParams{
		Threshold: u,
		Xi:        xi,
		Beta:      beta,
		Nu:        nu,
		N:         n,
	}
	e.fitted = true
	return nil
}

// VaR calculates the Value-at-Risk at confidence level 1 - alpha (e.g., alpha = 0.01 for 99% VaR)
// under the fitted POT model:
//
//	VaR_{1-\alpha} = u + (\beta / \xi) * ( ((N / N_u) * \alpha)^{-\xi} - 1 )
//
// 0 B/op, 0 allocs/op.
func (e *EVTRiskEngine) VaR(alpha float64) float64 {
	if !e.fitted || alpha <= 0 || alpha >= 1.0 {
		return 0
	}
	p := e.Params
	probRatio := (float64(p.N) / float64(p.Nu)) * alpha
	if probRatio <= 0 {
		return p.Threshold
	}

	if math.Abs(p.Xi) < 1e-6 {
		// Limit as \xi -> 0: GPD becomes Exponential(beta)
		// VaR = u - \beta * ln((N/N_u) * alpha)
		return p.Threshold - p.Beta*math.Log(probRatio)
	}

	return p.Threshold + (p.Beta/p.Xi)*(math.Pow(probRatio, -p.Xi)-1.0)
}

// ExpectedShortfall calculates the Expected Shortfall (CVaR) at confidence level 1 - alpha:
//
//	ES_{1-\alpha} = \frac{VaR_{1-\alpha}}{1 - \xi} + \frac{\beta - \xi * u}{1 - \xi}
//
// 0 B/op, 0 allocs/op.
func (e *EVTRiskEngine) ExpectedShortfall(alpha float64) float64 {
	if !e.fitted {
		return 0
	}
	varAlpha := e.VaR(alpha)
	p := e.Params
	oneMinusXi := 1.0 - p.Xi
	if math.Abs(oneMinusXi) < 1e-6 {
		oneMinusXi = 1e-6
	}

	return varAlpha/oneMinusXi + (p.Beta-p.Xi*p.Threshold)/oneMinusXi
}

// BreakerState models the operational state of the flash-crash circuit breaker.
type BreakerState uint8

const (
	StateNormal BreakerState = iota
	StateCaution
	StateHalted
)

func (s BreakerState) String() string {
	switch s {
	case StateNormal:
		return "Normal"
	case StateCaution:
		return "Caution"
	case StateHalted:
		return "Halted"
	default:
		return "Unknown"
	}
}

// MicrostructureCircuitBreaker is an institutional flash-crash circuit breaker
// monitoring high-frequency microstructure anomalies:
//  1. Top-5 depth collapse (> 80% drop within 500ms).
//  2. Spread blowout (> 5x median spread).
//  3. Severe VPIN spike (> 0.85).
//
// 0 B/op, 0 allocs/op.
type MicrostructureCircuitBreaker struct {
	State        BreakerState
	MedianSpread float64
	PeakDepth    float64
	LastTimeNs   int64
	PeakTimeNs   int64

	// Thresholds
	DepthDropThreshold float64 // default 0.80 (80% drop)
	SpreadMultLimit    float64 // default 5.0 (5x median spread)
	VPINThreshold      float64 // default 0.85
	TimeWindowNs       int64   // default 500ms (500_000_000 ns)
}

// NewMicrostructureCircuitBreaker initializes a circuit breaker with baseline median spread.
func NewMicrostructureCircuitBreaker(medianSpread float64) *MicrostructureCircuitBreaker {
	if medianSpread <= 0 {
		medianSpread = 1.0
	}
	return &MicrostructureCircuitBreaker{
		State:              StateNormal,
		MedianSpread:       medianSpread,
		DepthDropThreshold: 0.80,
		SpreadMultLimit:    5.0,
		VPINThreshold:      0.85,
		TimeWindowNs:       500_000_000, // 500 ms
	}
}

// Check evaluates the circuit breaker condition without timestamp, or using internal clock.
// Executes in 0 B/op, 0 allocs/op.
func (cb *MicrostructureCircuitBreaker) Check(spread, top5Depth, vpin float64) (halted bool, reason string) {
	return cb.CheckWithTime(spread, top5Depth, vpin, cb.LastTimeNs+100_000_000)
}

// CheckWithTime updates book microstructure state and evaluates circuit breaker triggers:
//  1. Top-5 depth collapse (> 80% drop within 500ms window).
//  2. Spread blowout (> 5x median spread).
//  3. Severe VPIN spike (> 0.85).
//
// Executes in 0 B/op, 0 allocs/op.
func (cb *MicrostructureCircuitBreaker) CheckWithTime(spread, top5Depth, vpin float64, timeNs int64) (halted bool, reason string) {
	cb.LastTimeNs = timeNs

	// Update rolling peak depth within 500ms window
	if timeNs-cb.PeakTimeNs > cb.TimeWindowNs || top5Depth >= cb.PeakDepth {
		cb.PeakDepth = top5Depth
		cb.PeakTimeNs = timeNs
	}

	// 1. Check Top-5 depth collapse
	if cb.PeakDepth > 1e-6 {
		depthDrop := (cb.PeakDepth - top5Depth) / cb.PeakDepth
		if depthDrop >= cb.DepthDropThreshold && (timeNs-cb.PeakTimeNs) <= cb.TimeWindowNs {
			cb.State = StateHalted
			return true, "depth_collapse_80pct"
		}
	}

	// 2. Check spread blowout
	if cb.MedianSpread > 1e-6 && spread >= cb.SpreadMultLimit*cb.MedianSpread {
		cb.State = StateHalted
		return true, "spread_blowout_5x"
	}

	// 3. Check severe VPIN spike
	if vpin >= cb.VPINThreshold {
		cb.State = StateHalted
		return true, "vpin_spike_85"
	}

	// Caution zone checks (e.g. 3x spread or 0.70 VPIN or 60% depth drop)
	if (cb.MedianSpread > 1e-6 && spread >= 3.0*cb.MedianSpread) || vpin >= 0.70 {
		cb.State = StateCaution
		return false, ""
	}

	cb.State = StateNormal
	return false, ""
}

// Reset clears the circuit breaker back to Normal state.
func (cb *MicrostructureCircuitBreaker) Reset(medianSpread float64) {
	if medianSpread > 0 {
		cb.MedianSpread = medianSpread
	}
	cb.State = StateNormal
	cb.PeakDepth = 0
	cb.PeakTimeNs = 0
	cb.LastTimeNs = 0
}
