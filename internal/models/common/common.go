// Package common defines the interface every forecasting model implements and
// the shared numerical machinery they use.
//
// A model answers one question: "given the features observable right now, what
// is the expected change in the mid price over the next H seconds, and how
// confident am I?" It never says what to trade, and it may not look at the
// future: Forecast receives only the feature vector for the current instant.
//
// The evaluator, not the model, decides what a forecast is worth. A model that
// reports a confident number still scores as the evaluator's metrics show.
package common

import (
	"errors"
	"fmt"
	"math"
	"time"

	"piquant-live/internal/features"
)

// Horizon is a forecast horizon. The set is fixed so results are comparable
// across models.
type Horizon int

// Supported horizons.
const (
	H30s Horizon = 0
	H1m  Horizon = 1
	H5m  Horizon = 2
)

// Horizons lists every supported horizon in order.
var Horizons = []Horizon{H30s, H1m, H5m}

// Duration returns the horizon's length.
func (h Horizon) Duration() time.Duration {
	switch h {
	case H30s:
		return 30 * time.Second
	case H1m:
		return time.Minute
	case H5m:
		return 5 * time.Minute
	}
	return 0
}

// Seconds returns the horizon length in seconds.
func (h Horizon) Seconds() float64 { return h.Duration().Seconds() }

// String names the horizon for reports and config.
func (h Horizon) String() string {
	switch h {
	case H30s:
		return "30s"
	case H1m:
		return "1m"
	case H5m:
		return "5m"
	}
	return fmt.Sprintf("horizon(%d)", int(h))
}

// ParseHorizon parses "30s", "1m" or "5m".
func ParseHorizon(s string) (Horizon, error) {
	switch s {
	case "30s", "30":
		return H30s, nil
	case "1m", "60s", "60":
		return H1m, nil
	case "5m", "300s", "300":
		return H5m, nil
	}
	return 0, fmt.Errorf("common: unknown horizon %q (want 30s, 1m or 5m)", s)
}

// Forecast is one model's estimate at one instant.
//
// Value is the expected change in mid price over the horizon, in basis points:
// predicted_value = 1e4 * (mid(t+H) - mid(t)) / mid(t). It is a statistical
// estimate, not an instruction.
type Forecast struct {
	Model   string // model name
	Horizon Horizon
	Symbol  uint16  // engine symbol id
	At      int64   // event time of the forecast, ns
	Value   float64 // expected mid move, bps

	// Confidence is a model-internal uncertainty measure, [0, 1] where 1 is
	// most confident. It is reported and used for weighting, never assumed to
	// be calibrated; the evaluator measures calibration separately.
	Confidence float64
	// Valid is false when the model could not produce a usable estimate.
	Valid bool
	// Reason explains an invalid forecast. Empty when Valid.
	Reason string
	// Cost is the wall time spent producing this forecast.
	Cost time.Duration
}

// Model is the shared interface. Implementations must be deterministic and
// allocation-free in Forecast once fitted.
type Model interface {
	// Name is stable across runs and versions.
	Name() string
	// Version identifies the fitted parameters' provenance.
	Version() string
	// Features lists the feature-vector indices the model reads.
	Features() []int
	// Horizons lists the horizons the model supports.
	Horizons() []Horizon
	// Forecast produces one estimate from the current feature vector.
	Forecast(f []float32, sym uint16, at int64, h Horizon) Forecast
	// Cost returns cumulative inference cost since construction.
	Cost() time.Duration
}

// Baseline names the simple references every model must beat.
type Baseline string

// Baselines.
const (
	BaselineZero    Baseline = "zero"
	BaselineLastRet Baseline = "last-return"
	BaselineMean    Baseline = "rolling-mean"
	BaselineLinear  Baseline = "ridge"
)

// FeatureCount is the width of a feature vector.
const FeatureCount = features.NCore

// Dot16 computes the inner product w^T x for 16-element feature vectors
// unrolled into four parallel 4-lane chunks (SIMD / auto-vectorization friendly).
//
// Micro-architecture details for ARM64 Cortex-A76 (BCM2712):
// 1. Dual-issue vector floating-point pipelines: Cortex-A76 features two symmetric
//    128-bit vector/FP execution pipelines (V0 and V1). By maintaining four independent
//    accumulators (acc0, acc1, acc2, acc3), we break the serial latency dependency
//    chain of floating point addition, allowing dual-issue FMA / vector instructions
//    to dispatch simultaneously without stalling the pipeline.
// 2. Cache-line alignment: A 16-element float64 array is exactly 128 bytes (2 x 64-byte
//    cache lines). Each 4-lane chunk is 32 bytes (half a cache line). Linear consecutive
//    loads map directly to paired 128-bit vector registers (LDP on ARM64) without
//    incurring unaligned split cache-line penalty.
// 3. Guaranteed zero heap allocations (0 B/op, 0 allocs/op).
func Dot16(w, x *[FeatureCount]float64) float64 {
	acc0 := w[0]*x[0] + w[1]*x[1] + w[2]*x[2] + w[3]*x[3]
	acc1 := w[4]*x[4] + w[5]*x[5] + w[6]*x[6] + w[7]*x[7]
	acc2 := w[8]*x[8] + w[9]*x[9] + w[10]*x[10] + w[11]*x[11]
	acc3 := w[12]*x[12] + w[13]*x[13] + w[14]*x[14] + w[15]*x[15]
	return (acc0 + acc1) + (acc2 + acc3)
}

// ErrNotFitted is returned when a model is asked to forecast before fitting.
var ErrNotFitted = errors.New("common: model is not fitted")

// ValidFeature reports whether idx names a feature this build produces.
func ValidFeature(idx int) bool { return idx >= 0 && idx < FeatureCount }

// Bps converts a mid change into basis points.
func Bps(from, to float64) float64 {
	if from <= 0 {
		return 0
	}
	return (to - from) / from * 1e4
}

// Finite reports whether v is usable as a model output.
func Finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
