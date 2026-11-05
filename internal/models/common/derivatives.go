package common

import "math"

// PerpBasisTracker tracks synthetic/inferred perpetual futures basis premium/discount
// (from microprice divergence, multi-level OFI persistence, and trade arrival flow),
// funding-settlement pull, and rolling Welford/EWMA basis z-score in O(1) time.
type PerpBasisTracker struct {
	Decay         float64 // EWMA smoothing factor in (0, 1)
	FundingWeight float64 // Strength of funding-settlement mean-reversion pull
	emaBasisBps   float64
	emaVarBpsSq   float64
	initialized   bool
}

// NewPerpBasisTracker constructs an O(1) perpetual basis and funding-skew tracker.
func NewPerpBasisTracker(decay, fundingWeight float64) *PerpBasisTracker {
	if decay <= 0 || decay >= 1.0 {
		decay = 0.08
	}
	if fundingWeight <= 0 {
		fundingWeight = 0.25
	}
	return &PerpBasisTracker{
		Decay:         decay,
		FundingWeight: fundingWeight,
		emaVarBpsSq:   1.0,
	}
}

// Update ingests a 16-element core feature vector and returns:
//   - basisBps: instantaneous inferred perpetual-to-spot basis premium/discount in bps
//   - basisZScore: standardized basis dislocation z-score
//   - fundingCarryBps: expected funding-rate settlement pull / carry alpha in bps
//
// Zero heap allocations.
func (p *PerpBasisTracker) Update(x *[FeatureCount]float64) (basisBps, basisZScore, fundingCarryBps float64) {
	// Feature indices (invariant 0..15):
	// x[0]=imb_l1, x[2]=imb_l20, x[3]=micro_dev, x[4]=spread_bps,
	// x[6]=ofi_5s, x[7]=ofi_30s, x[9]=mlofi_5s, x[12]=ret_5s_bps, x[13]=trade_imb_5s
	spread := math.Max(0.1, x[4])
	microBasis := x[3] * spread * 1.4
	ofiPersistence := 0.18*x[6] + 0.08*x[7] + 0.22*x[9]
	flowSkew := 0.35*x[13] + 0.15*x[12] + 0.40*(x[0]-x[2])

	basisBps = microBasis + ofiPersistence + flowSkew

	if !p.initialized {
		p.emaBasisBps = basisBps
		p.emaVarBpsSq = math.Max(0.25, basisBps*basisBps)
		p.initialized = true
	} else {
		diff := basisBps - p.emaBasisBps
		p.emaBasisBps += p.Decay * diff
		p.emaVarBpsSq = (1.0-p.Decay)*p.emaVarBpsSq + p.Decay*(diff*diff)
	}

	std := math.Sqrt(math.Max(1e-6, p.emaVarBpsSq))
	basisZScore = (basisBps - p.emaBasisBps) / std
	if basisZScore > 6.0 {
		basisZScore = 6.0
	} else if basisZScore < -6.0 {
		basisZScore = -6.0
	}

	// Funding settlement pull: when perpetual basis is stretched beyond +/-1.2 sigma,
	// funding arbitrageurs pull basis back toward par while earning carry.
	fundingCarryBps = -p.FundingWeight * basisBps * math.Tanh(0.5*math.Abs(basisZScore))
	return basisBps, basisZScore, fundingCarryBps
}

// EvaluateStateless computes instantaneous basis, z-score against training baseline, and funding carry
// without mutating state (used inside allocation-free Predict hot paths).
func (p *PerpBasisTracker) EvaluateStateless(x *[FeatureCount]float64) (basisBps, basisZScore, fundingCarryBps float64) {
	spread := math.Max(0.1, x[4])
	microBasis := x[3] * spread * 1.4
	ofiPersistence := 0.18*x[6] + 0.08*x[7] + 0.22*x[9]
	flowSkew := 0.35*x[13] + 0.15*x[12] + 0.40*(x[0]-x[2])
	basisBps = microBasis + ofiPersistence + flowSkew

	std := math.Sqrt(math.Max(0.25, p.emaVarBpsSq))
	basisZScore = (basisBps - p.emaBasisBps) / std
	if basisZScore > 6.0 {
		basisZScore = 6.0
	} else if basisZScore < -6.0 {
		basisZScore = -6.0
	}
	fundingCarryBps = -p.FundingWeight * basisBps * math.Tanh(0.5*math.Abs(basisZScore))
	return basisBps, basisZScore, fundingCarryBps
}

// LiquidationCascadeDetector identifies forced margin liquidation cascades and
// distinguishes between early cascade continuation vs post-cascade V-shaped exhaustion reversion.
type LiquidationCascadeDetector struct {
	VelocityThreshold  float64 // 1s return velocity / arrival threshold (f[11], f[14])
	DepletionThreshold float64 // Book depletion / 30s OFI thinning threshold (f[7])
	ImbalanceThreshold float64 // Unidirectional flow imbalance threshold (f[8], f[13])
	VolSpikeThreshold  float64 // Realized volatility / flow spike threshold (f[10], f[13])
}

// NewLiquidationCascadeDetector returns a calibrated liquidation cascade detector.
func NewLiquidationCascadeDetector() *LiquidationCascadeDetector {
	return &LiquidationCascadeDetector{
		VelocityThreshold:  3.0,
		DepletionThreshold: 2.5,
		ImbalanceThreshold: 1.2,
		VolSpikeThreshold:  2.0,
	}
}

// Evaluate inspects the 16-feature vector for forced liquidation signatures:
//   - high trade arrival / velocity (f[11], f[14])
//   - sharp book depletion/thinning (f[7], f[4])
//   - extreme unidirectional trade/OFI imbalance (f[8], f[13])
//   - volatility spike (f[10], f[13])
//
// Returns cascadeIntensity in [0, 1], alphaSignalBps (continuation in early cascade,
// V-shaped mean-reversion at exhaustion), and whether a cascade regime is active.
// Zero heap allocations.
func (d *LiquidationCascadeDetector) Evaluate(x *[FeatureCount]float64) (cascadeIntensity float64, alphaSignalBps float64, inCascade bool) {
	vThresh := d.VelocityThreshold
	if vThresh <= 0 {
		vThresh = 3.0
	}
	depThresh := d.DepletionThreshold
	if depThresh <= 0 {
		depThresh = 2.5
	}
	imbThresh := d.ImbalanceThreshold
	if imbThresh <= 0 {
		imbThresh = 1.2
	}
	volThresh := d.VolSpikeThreshold
	if volThresh <= 0 {
		volThresh = 2.0
	}

	// Signals specified in Deliverable 3:
	// f[11] (ret_1s_bps / arrival velocity), f[7] (ofi_30s book depletion),
	// f[8] (mlofi_1s unidirectional imbalance), f[13] (trade_imb_5s / vol spike),
	// plus f[10] (vol_30s_bps) and f[14] (trade_cnt_1s).
	velScore := (math.Abs(x[11]) + 0.25*math.Max(0, x[14])) / vThresh
	depletionScore := (math.Abs(x[7]) + 0.5*math.Max(0, x[4]-1.0)) / depThresh
	unidirScore := (math.Abs(x[8]) + math.Abs(x[13])) / imbThresh
	volScore := (math.Abs(x[13]) + math.Max(0, x[10])) / volThresh

	raw := 0.30*velScore + 0.25*depletionScore + 0.25*unidirScore + 0.20*volScore
	cascadeIntensity = 1.0 / (1.0 + math.Exp(-1.6*(raw-1.15)))
	inCascade = cascadeIntensity >= 0.55

	// Determine direction of forced liquidation sweep (-1 for long liquidations / sell sweep, +1 for short squeeze)
	flowDir := x[8] + x[11] + x[13]
	sweepSign := 1.0
	if flowDir < 0 {
		sweepSign = -1.0
	}

	// Exhaustion vs early continuation:
	// When L1 touch imbalance (x[0]) or microprice (x[3]) starts diverging against the 30s depletion (x[7])
	// or when cascade intensity reaches climax (> 0.72), forced sellers are exhausted -> strong V-shaped reversion!
	// During early cascade initiation (0.55 <= intensity <= 0.72 and L1 aligned with sweep), momentum continues briefly.
	touchAbsorption := (-sweepSign * (x[0] + 2.0*x[3]))
	if cascadeIntensity > 0.72 || touchAbsorption > 0.15 {
		// V-shaped post-cascade exhaustion reversion against the forced sweep
		reversionMag := cascadeIntensity * (1.8 + 0.6*math.Max(0, touchAbsorption)) * math.Min(4.0, math.Max(0.5, math.Abs(x[11])*0.4+1.0))
		alphaSignalBps = -sweepSign * reversionMag
	} else if inCascade {
		// Early cascade momentum continuation
		alphaSignalBps = sweepSign * cascadeIntensity * 0.65
	} else {
		alphaSignalBps = -sweepSign * cascadeIntensity * 0.20 * touchAbsorption
	}

	return cascadeIntensity, alphaSignalBps, inCascade
}

// PerpLiquidationReversionModel fits a regularized Ridge base model augmented with
// non-linear liquidation-exhaustion V-reversion and perpetual basis carry adjustment.
// Predict is 100% allocation-free (0 allocs/op).
type PerpLiquidationReversionModel struct {
	BaseRidge   Ridge
	Basis       PerpBasisTracker
	Detector    LiquidationCascadeDetector
	ReversionWt float64
	BasisWt     float64
	fitted      bool
}

// NewPerpLiquidationReversionModel constructs a PerpLiquidationReversionModel.
func NewPerpLiquidationReversionModel(lambda float64) *PerpLiquidationReversionModel {
	if lambda <= 0 {
		lambda = 1e-3
	}
	return &PerpLiquidationReversionModel{
		BaseRidge:   Ridge{Lam: lambda},
		Basis:       *NewPerpBasisTracker(0.08, 0.25),
		Detector:    *NewLiquidationCascadeDetector(),
		ReversionWt: 0.35,
		BasisWt:     0.25,
	}
}

// Name returns the canonical model identifier.
func (m *PerpLiquidationReversionModel) Name() string {
	return "perp_liquidation_reversion"
}

// Fit trains the regularized Ridge base model and calibrates the non-linear liquidation
// reversion and perpetual basis carry weights on the training residuals.
func (m *PerpLiquidationReversionModel) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	if len(idx) == 0 {
		idx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}
	if err := m.BaseRidge.Fit(samples, idx); err != nil {
		return err
	}

	m.Basis = *NewPerpBasisTracker(0.08, 0.25)
	m.Detector = *NewLiquidationCascadeDetector()

	// Calibrate Detector thresholds and non-linear residual weights from training distribution
	var sumVel, sumDep, sumImb, sumVol float64
	for i := range samples {
		x := &samples[i].X
		sumVel += math.Abs(x[11]) + 0.25*math.Max(0, x[14])
		sumDep += math.Abs(x[7]) + 0.5*math.Max(0, x[4]-1.0)
		sumImb += math.Abs(x[8]) + math.Abs(x[13])
		sumVol += math.Abs(x[13]) + math.Max(0, x[10])
		m.Basis.Update(x)
	}
	n := float64(len(samples))
	if n > 0 {
		m.Detector.VelocityThreshold = math.Max(1.0, (sumVel/n)*1.35)
		m.Detector.DepletionThreshold = math.Max(0.8, (sumDep/n)*1.35)
		m.Detector.ImbalanceThreshold = math.Max(0.5, (sumImb/n)*1.35)
		m.Detector.VolSpikeThreshold = math.Max(0.8, (sumVol/n)*1.35)
	}

	// Least-squares calibration of ReversionWt and BasisWt on residuals r_i = y_i - yHat_ridge(x_i)
	var sRevSq, sBasSq, sRevY, sBasY float64
	for i := range samples {
		x := &samples[i].X
		res := samples[i].Y - m.BaseRidge.Predict(x)
		_, revAlpha, _ := m.Detector.Evaluate(x)
		_, _, carryAlpha := m.Basis.EvaluateStateless(x)
		sRevSq += revAlpha * revAlpha
		sRevY += revAlpha * res
		sBasSq += carryAlpha * carryAlpha
		sBasY += carryAlpha * res
	}
	if sRevSq > 1e-6 {
		w := sRevY / (sRevSq + 1.0)
		m.ReversionWt = math.Max(0.05, math.Min(0.85, w))
	} else {
		m.ReversionWt = 0.25
	}
	if sBasSq > 1e-6 {
		w := sBasY / (sBasSq + 1.0)
		m.BasisWt = math.Max(0.05, math.Min(0.65, w))
	} else {
		m.BasisWt = 0.20
	}

	m.fitted = true
	return nil
}

// Predict computes the forecast in basis points with zero heap allocations.
func (m *PerpLiquidationReversionModel) Predict(x *[FeatureCount]float64) float64 {
	if !m.fitted {
		return 0
	}
	base := m.BaseRidge.Predict(x)
	_, revAlpha, _ := m.Detector.Evaluate(x)
	_, _, carryAlpha := m.Basis.EvaluateStateless(x)
	return base + m.ReversionWt*revAlpha + m.BasisWt*carryAlpha
}

// Clone returns a deep, independent copy of the model.
func (m *PerpLiquidationReversionModel) Clone() *PerpLiquidationReversionModel {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}
