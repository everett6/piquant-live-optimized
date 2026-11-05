package common

import (
	"math"
	"testing"
)

func TestElasticNetConvergence(t *testing.T) {
	// Generate synthetic linear data with 2 informative features and 2 noise features
	samples := make([]Sample, 200)
	for i := 0; i < 200; i++ {
		x0 := float64(i%10) - 4.5
		x1 := float64((i*3)%13) - 6.0
		xNoise := float64((i*7)%5) - 2.0
		// True model: y = 2.0 * x0 - 1.5 * x1 + noise
		y := 2.0*x0 - 1.5*x1 + (float64(i%3)-1.0)*0.1
		samples[i] = Sample{
			X: [FeatureCount]float64{x0, x1, xNoise, 0, 0, 0, 0, 0, 0, 0},
			Y: y,
		}
	}

	model := NewElasticNet(1e-3, 0.5)
	idx := []int{0, 1, 2}
	if err := model.Fit(samples, idx); err != nil {
		t.Fatalf("ElasticNet Fit failed: %v", err)
	}

	// Predict test point
	testSample := [FeatureCount]float64{2.0, -1.0, 5.0, 0, 0, 0, 0, 0, 0, 0}
	expectedY := 2.0*2.0 - 1.5*(-1.0) // 5.5
	pred := model.Predict(&testSample)

	if math.Abs(pred-expectedY) > 0.5 {
		t.Errorf("expected prediction near %.2f, got %.2f", expectedY, pred)
	}
}

func TestAdaptiveRLSTracking(t *testing.T) {
	samples := make([]Sample, 150)
	for i := 0; i < 150; i++ {
		x := float64(i) * 0.1
		// Slowly shifting slope
		slope := 1.5
		if i > 75 {
			slope = -1.0
		}
		y := slope * x
		samples[i] = Sample{
			X: [FeatureCount]float64{x, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			Y: y,
		}
	}

	model := NewAdaptiveRLS(0.95)
	idx := []int{0}
	if err := model.Fit(samples, idx); err != nil {
		t.Fatalf("AdaptiveRLS Fit failed: %v", err)
	}

	// RLS should have adapted to the recent negative slope
	testSample := [FeatureCount]float64{10.0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	pred := model.Predict(&testSample)
	if pred > 0 {
		t.Errorf("expected negative prediction after regime shift, got %.2f", pred)
	}
}
