package common

import (
	"math"
)

// MetaLabelingFilter applies Marcos López de Prado's two-stage meta-labeling.
// Stage 1: Base regression/direction model produces signal direction (+1 or -1).
// Stage 2: Calibrated secondary classifier estimates P(trade clears round-trip fee hurdle).
// A trade is taken only if conviction > probabilityThreshold, with position sized via fractional Kelly.
type MetaLabelingFilter struct {
	BaseModel    ContinuousModel
	MetaModel    *Logistic
	FeeHurdleBps float64
	Threshold    float64
	Fitted       bool
}

// ContinuousModel interface for stage 1 predictors.
type ContinuousModel interface {
	Fit(samples []Sample, idx []int) error
	Predict(x *[FeatureCount]float64) float64
}

// NewMetaLabelingFilter constructs a meta-labeling overlay.
func NewMetaLabelingFilter(base ContinuousModel, feeHurdleBps, probThreshold float64) *MetaLabelingFilter {
	if feeHurdleBps <= 0 {
		feeHurdleBps = 15.0 // default 15 bps round-trip taker hurdle
	}
	if probThreshold <= 0.5 {
		probThreshold = 0.55 // need positive expectation
	}
	return &MetaLabelingFilter{
		BaseModel:    base,
		MetaModel:    NewLogistic(1e-3),
		FeeHurdleBps: feeHurdleBps,
		Threshold:    probThreshold,
	}
}

// Fit trains both the primary directional predictor and the secondary meta-classifier.
func (m *MetaLabelingFilter) Fit(samples []Sample, idx []int) error {
	if len(samples) < 30 {
		return ErrNotFitted
	}

	// 1. Fit primary model
	if err := m.BaseModel.Fit(samples, idx); err != nil {
		return err
	}

	// 2. Form meta-labels: 1 if |actual| > FeeHurdle and direction matches; 0 otherwise
	metaSamples := make([]Sample, len(samples))
	for i, s := range samples {
		p := m.BaseModel.Predict(&s.X)
		metaSamples[i] = s

		isCorrectDirection := (p > 0 && s.Y > 0) || (p < 0 && s.Y < 0)
		clearsFees := math.Abs(s.Y) >= m.FeeHurdleBps

		if isCorrectDirection && clearsFees {
			metaSamples[i].Y = 1.0
		} else {
			metaSamples[i].Y = 0.0
		}
	}

	// 3. Fit secondary meta-classifier
	if err := m.MetaModel.Fit(metaSamples, idx); err != nil {
		return err
	}

	m.Fitted = true
	return nil
}

// Predict outputs zero (no trade) if conviction does not clear the fee hurdle,
// or the Kelly-sized directional forecast if confidence is high.
func (m *MetaLabelingFilter) Predict(x *[FeatureCount]float64) float64 {
	if !m.Fitted {
		return 0
	}

	primaryVal := m.BaseModel.Predict(x)
	if primaryVal == 0 {
		return 0
	}

	// Probability of clearing fee hurdle
	prob := m.MetaModel.Prob(x)
	if prob < m.Threshold {
		return 0 // FILTER OUT: avoid bleeding taker fees on noisy signals
	}

	// Half-Kelly bet sizing scaling
	kellyFraction := math.Max(0.1, 2.0*prob-1.0)
	return primaryVal * kellyFraction
}

// PredictWithMeta returns directional prediction, execution conviction probability, and decision.
func (m *MetaLabelingFilter) PredictWithMeta(x *[FeatureCount]float64) (pred float64, prob float64, takeTrade bool) {
	if !m.Fitted {
		return 0, 0, false
	}
	primaryVal := m.BaseModel.Predict(x)
	prob = m.MetaModel.Prob(x)
	takeTrade = prob >= m.Threshold && math.Abs(primaryVal) > 0.01
	if !takeTrade {
		return 0, prob, false
	}
	kellyFraction := math.Max(0.1, 2.0*prob-1.0)
	return primaryVal * kellyFraction, prob, true
}
