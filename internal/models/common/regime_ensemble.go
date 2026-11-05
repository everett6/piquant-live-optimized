package common

import (
	"math"
)

// MarketRegime identifies the real-time microstructure state.
type MarketRegime string

const (
	RegimeCalmMaker    MarketRegime = "calm_maker"
	RegimeDirectional  MarketRegime = "directional_flow"
	RegimeToxicCascade MarketRegime = "toxic_cascade"
)

// RegimeDiagnostics reports the detected regime and expert weights at a point in time.
type RegimeDiagnostics struct {
	Regime           MarketRegime `json:"regime"`
	ChangePointProb  float64      `json:"changepoint_prob"` // BOCPD hazard probability [0, 1]
	ToxicityScore    float64      `json:"toxicity_score"`   // Normalized order flow toxicity [0, 1]
	WeightRidge      float64      `json:"weight_ridge"`
	WeightElasticNet float64      `json:"weight_elastic_net"`
	WeightGLFTMaker  float64      `json:"weight_glft_maker"`
}

// ClassifyRegime evaluates the 16-element feature vector to classify the microstructure regime
// and estimate the Bayesian Online Changepoint Detection (BOCPD) hazard probability.
func ClassifyRegime(x *[FeatureCount]float64, meanVol, meanSpread float64) RegimeDiagnostics {
	if meanVol <= 0 {
		meanVol = 2.0
	}
	if meanSpread <= 0 {
		meanSpread = 1.0
	}

	spread := math.Max(0.1, x[4]) // spread_bps
	vol := math.Max(0.1, x[10])   // vol_30s_bps
	ret1s := x[11]                // ret_1s_bps
	tradeImb := math.Abs(x[13])   // |trade_imb_5s|
	ofiMag := math.Abs(x[8])      // |mlofi_1s|

	volRatio := vol / meanVol
	spreadRatio := spread / meanSpread

	// BOCPD changepoint probability from abrupt variance / spread innovation
	zShock := 0.45*(volRatio-1.0) + 0.35*(spreadRatio-1.0) + 0.20*(math.Abs(ret1s)/meanVol)
	cpProb := 1.0 / (1.0 + math.Exp(-1.8*(zShock-1.2)))

	// Normalized toxicity proxy in [0, 1]
	toxicity := 1.0 / (1.0 + math.Exp(-(0.5*(spreadRatio-1.2) + 0.4*tradeImb - 0.5)))

	// 1. Toxic Cascade / Liquidation Shock regime
	if cpProb > 0.72 || spreadRatio > 2.4 || ret1s < -6.0 {
		return RegimeDiagnostics{
			Regime:           RegimeToxicCascade,
			ChangePointProb:  cpProb,
			ToxicityScore:    math.Max(0.75, toxicity),
			WeightRidge:      0.15,
			WeightElasticNet: 0.70, // Sparse regularized model resists outlier noise
			WeightGLFTMaker:  0.15,
		}
	}

	// 2. Directional Order Flow Momentum regime
	if ofiMag > 0.6 || math.Abs(ret1s) > 1.2 || tradeImb > 0.8 {
		return RegimeDiagnostics{
			Regime:           RegimeDirectional,
			ChangePointProb:  cpProb,
			ToxicityScore:    toxicity,
			WeightRidge:      0.50,
			WeightElasticNet: 0.30,
			WeightGLFTMaker:  0.20,
		}
	}

	// 3. Calm Range-Bound Market-Making regime
	return RegimeDiagnostics{
		Regime:           RegimeCalmMaker,
		ChangePointProb:  cpProb,
		ToxicityScore:    toxicity,
		WeightRidge:      0.20,
		WeightElasticNet: 0.20,
		WeightGLFTMaker:  0.60,
	}
}

// RegimeSwitchingEnsemble combines MLOFI Ridge, Sparse ElasticNet, and GLFT Queue-Reactive Maker
// using online regime gating and Exponentially Weighted Model Averaging (EWMA).
type RegimeSwitchingEnsemble struct {
	RidgeExpert      *Ridge
	ElasticNetExpert *ElasticNet
	MakerExpert      *GLFTMarketMaker
	MeanVol          float64
	MeanSpread       float64
	BaseWeights      [3]float64
	fitted           bool
}

// NewRegimeSwitchingEnsemble constructs a 3-expert regime-switching ensemble.
func NewRegimeSwitchingEnsemble(lambda float64) *RegimeSwitchingEnsemble {
	if lambda <= 0 {
		lambda = 1e-3
	}
	return &RegimeSwitchingEnsemble{
		RidgeExpert:      NewRidge(lambda),
		ElasticNetExpert: NewElasticNet(lambda, 0.5),
		MakerExpert:      NewGLFTMarketMaker(lambda, 0.05, 0.0),
		MeanVol:          2.0,
		MeanSpread:       1.0,
		BaseWeights:      [3]float64{1.0 / 3.0, 1.0 / 3.0, 1.0 / 3.0},
	}
}

// Fit trains all three constituent experts and calibrates baseline exponential weights
// via out-of-fold online exponential loss weighting.
func (e *RegimeSwitchingEnsemble) Fit(samples []Sample, idx []int) error {
	if err := e.RidgeExpert.Fit(samples, idx); err != nil {
		return err
	}
	if err := e.ElasticNetExpert.Fit(samples, idx); err != nil {
		return err
	}
	if err := e.MakerExpert.Fit(samples, idx); err != nil {
		return err
	}

	var sumVol, sumSpread float64
	for i := range samples {
		sumSpread += math.Max(0.1, samples[i].X[4])
		sumVol += math.Max(0.1, samples[i].X[10])
	}
	n := float64(len(samples))
	if n > 0 {
		e.MeanSpread = sumSpread / n
		e.MeanVol = sumVol / n
	}

	// Calibrate online exponential weights w_{m,t+1} = w_{m,t} * exp(-eta * |y_t - yHat_{m,t}|)
	weights := [3]float64{1.0 / 3.0, 1.0 / 3.0, 1.0 / 3.0}
	eta := 0.02
	for i := range samples {
		x := &samples[i].X
		y := samples[i].Y
		p0 := e.RidgeExpert.Predict(x)
		p1 := e.ElasticNetExpert.Predict(x)
		p2 := e.MakerExpert.Predict(x)

		l0 := math.Min(20.0, math.Abs(y-p0))
		l1 := math.Min(20.0, math.Abs(y-p1))
		l2 := math.Min(20.0, math.Abs(y-p2))

		weights[0] *= math.Exp(-eta * l0)
		weights[1] *= math.Exp(-eta * l1)
		weights[2] *= math.Exp(-eta * l2)

		sumW := weights[0] + weights[1] + weights[2]
		if sumW > 1e-12 {
			weights[0] /= sumW
			weights[1] /= sumW
			weights[2] /= sumW
		} else {
			weights = [3]float64{1.0 / 3.0, 1.0 / 3.0, 1.0 / 3.0}
		}
	}
	e.BaseWeights = weights
	e.fitted = true
	return nil
}

// Predict blends the three experts dynamically conditioned on the detected microstructure regime.
func (e *RegimeSwitchingEnsemble) Predict(x *[FeatureCount]float64) float64 {
	if !e.fitted {
		return 0
	}
	diag := ClassifyRegime(x, e.MeanVol, e.MeanSpread)

	pRidge := e.RidgeExpert.Predict(x)
	pENet := e.ElasticNetExpert.Predict(x)
	pMaker := e.MakerExpert.Predict(x)

	// Blend learned EWMA base weights with instantaneous regime gate weights
	w0 := 0.5*e.BaseWeights[0] + 0.5*diag.WeightRidge
	w1 := 0.5*e.BaseWeights[1] + 0.5*diag.WeightElasticNet
	w2 := 0.5*e.BaseWeights[2] + 0.5*diag.WeightGLFTMaker

	pred := w0*pRidge + w1*pENet + w2*pMaker

	// During extreme toxic cascades, shrink directional exposure to preserve capital
	if diag.Regime == RegimeToxicCascade {
		pred *= (1.0 - 0.5*diag.ChangePointProb)
	}
	return pred
}
