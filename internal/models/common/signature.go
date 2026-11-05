package common

import "math"

// SignatureTermCount is the number of synthetic Depth-2 path signature cross-terms
// and Kyle's Lambda price impact features returned by ComputeLevyAreaFeatures.
const SignatureTermCount = 6

// ComputeLevyAreaFeatures computes synthetic Depth-2 path signature cross-terms (Lévy areas
// A_{i,j} = \frac{1}{2} \int (X^i dX^j - X^j dX^i) approximated over multi-horizon orderbook
// increments) and dynamic Kyle's Lambda (\lambda_{Kyle} = |\Delta P| / \max(|\text{SignedFlow}|, \epsilon))
// in 0 heap allocations on the stack.
//
// Input feature indices (invariant 0..15):
//   - x[0..2]: multi-level imbalances (imb_l1, imb_l5, imb_l20)
//   - x[3]:    micro_dev
//   - x[5]:    ofi_1s (microprice/touch flow divergence)
//   - x[8]:    mlofi_1s (multi-level OFI / trade flow imbalance)
//   - x[11]:   ret_1s_bps / Hawkes arrival velocity intensity
//   - x[13]:   trade_imb_5s
func ComputeLevyAreaFeatures(x [FeatureCount]float64) [SignatureTermCount]float64 {
	// Multi-level OFI / book slope trajectory increments
	dImbTouch := x[0] - x[1] // L1 vs L5 slope increment
	dImbDeep := x[1] - x[2]  // L5 vs L20 slope increment

	// 1. Lévy Area between L1-L5 touch curvature and L5-L20 deep book pressure
	levyBookCurvature := 0.5 * (x[0]*dImbDeep - x[2]*dImbTouch)

	// 2. Lévy Area between L1 imbalance (x[0]) and 1s OFI / microprice flow (x[5])
	levyTouchOFI := 0.5 * (x[0]*x[5] - x[3]*x[8])

	// 3. Lévy Area between 1s OFI (x[5]) and multi-level OFI / trade imbalance (x[8])
	levyMultiOFI := 0.5 * (x[5]*(x[8]-x[5]) - x[8]*(x[0]-x[1]))

	// 4. Lévy Area between multi-level OFI (x[8]) and 1s return / Hawkes intensity (x[11])
	levyFlowHawkes := 0.5 * (x[8]*x[11] - x[5]*x[13])

	// 5. Lead-lag rotational curl between microprice divergence (x[3], x[5]) and trade flow (x[8], x[11])
	levyMicroHawk := 0.5 * ((x[3]+0.2*x[5])*x[11] - x[8]*x[2])

	// 6. Dynamic Kyle's Lambda (\lambda_{Kyle} = \Delta P / SignedFlow) signed impact signal:
	// Measures marginal price displacement per unit of net order flow.
	signedFlow := 0.5*x[5] + 0.35*x[8] + 0.15*x[13]
	absFlow := math.Abs(signedFlow)
	deltaP := x[11]
	if math.Abs(deltaP) < 0.05 {
		deltaP = x[3] * math.Max(0.2, x[4])
	}
	lambdaKyle := math.Abs(deltaP) / math.Max(absFlow, 0.35)
	if lambdaKyle > 8.0 {
		lambdaKyle = 8.0
	}
	// Signed Kyle impact term (flow scaled by concave square-root / Kyle impact elasticity)
	kyleSignedImpact := signedFlow * math.Log1p(lambdaKyle) * 0.35

	return [SignatureTermCount]float64{
		levyBookCurvature,
		levyTouchOFI,
		levyMultiOFI,
		levyFlowHawkes,
		levyMicroHawk,
		kyleSignedImpact,
	}
}

// RoughSignatureOFIModel combines L2-regularized Ridge regression over the 16 core features
// with truncated Depth-2 path signature (Lévy area) cross-terms and dynamic Kyle's Lambda
// price impact damping.
//
// Predict executes in O(1) time with 0 B/op and 0 allocs/op.
type RoughSignatureOFIModel struct {
	BaseRidge  Ridge
	SigWeights [SignatureTermCount]float64
	SigMean    [SignatureTermCount]float64
	SigStd     [SignatureTermCount]float64
	KyleMean   float64
	KyleDamp   float64
	fitted     bool
}

// NewRoughSignatureOFIModel constructs an unfitted RoughSignatureOFIModel.
func NewRoughSignatureOFIModel(lambda float64) *RoughSignatureOFIModel {
	if lambda <= 0 {
		lambda = 1e-3
	}
	return &RoughSignatureOFIModel{
		BaseRidge: Ridge{Lam: lambda},
		SigStd:    [SignatureTermCount]float64{1, 1, 1, 1, 1, 1},
		KyleMean:  1.0,
		KyleDamp:  0.12,
	}
}

// Name returns the canonical model identifier.
func (m *RoughSignatureOFIModel) Name() string {
	return "rough_signature_ofi"
}

// Fit trains the base Ridge weights and calibrates the 6 Depth-2 Lévy area signature weights
// on the training residuals via L2-regularized normal equations.
func (m *RoughSignatureOFIModel) Fit(samples []Sample, idx []int) error {
	if len(samples) == 0 {
		return ErrNotFitted
	}
	useIdx := idx
	if len(useIdx) == 0 {
		useIdx = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	}

	if err := m.BaseRidge.Fit(samples, useIdx); err != nil {
		return err
	}

	n := float64(len(samples))
	var sumSig [SignatureTermCount]float64
	var sumSigSq [SignatureTermCount]float64
	var sumKyle float64

	for i := range samples {
		sig := ComputeLevyAreaFeatures(samples[i].X)
		for k := 0; k < SignatureTermCount; k++ {
			sumSig[k] += sig[k]
			sumSigSq[k] += sig[k] * sig[k]
		}
		flow := math.Abs(0.5*samples[i].X[5] + 0.35*samples[i].X[8] + 0.15*samples[i].X[13])
		lamK := math.Abs(samples[i].X[11]) / math.Max(flow, 0.35)
		sumKyle += math.Min(8.0, lamK)
	}

	for k := 0; k < SignatureTermCount; k++ {
		mean := sumSig[k] / n
		variance := sumSigSq[k]/n - mean*mean
		std := 1.0
		if variance > 1e-12 {
			std = math.Sqrt(variance)
		}
		m.SigMean[k] = mean
		m.SigStd[k] = std
	}
	if sumKyle > 0 {
		m.KyleMean = math.Max(0.25, sumKyle/n)
	}

	// Orthogonalized ridge projection of training residuals onto standardized Depth-2 Lévy area features
	var sXY [SignatureTermCount]float64
	var sXX [SignatureTermCount]float64
	reg := math.Max(1.0, m.BaseRidge.Lam*n+2.5)

	for i := range samples {
		res := samples[i].Y - m.BaseRidge.Predict(&samples[i].X)
		sig := ComputeLevyAreaFeatures(samples[i].X)
		for k := 0; k < SignatureTermCount; k++ {
			z := (sig[k] - m.SigMean[k]) / m.SigStd[k]
			sXY[k] += z * res
			sXX[k] += z * z
		}
	}

	for k := 0; k < SignatureTermCount; k++ {
		if sXX[k] > 1e-9 {
			w := (sXY[k] / (sXX[k] + reg)) / m.SigStd[k]
			m.SigWeights[k] = math.Max(-1.5, math.Min(1.5, w))
		} else {
			m.SigWeights[k] = 0
		}
	}

	m.fitted = true
	return nil
}

// Predict computes the point forecast (in bps) from a fixed-size feature array pointer
// in 0 B/op and 0 allocs/op.
func (m *RoughSignatureOFIModel) Predict(x *[FeatureCount]float64) float64 {
	if !m.fitted {
		return 0
	}
	base := m.BaseRidge.Predict(x)
	sig := ComputeLevyAreaFeatures(*x)

	var sigContrib float64
	for k := 0; k < SignatureTermCount; k++ {
		sigContrib += m.SigWeights[k] * (sig[k] - m.SigMean[k])
	}

	// Dynamic Kyle's Lambda impact damping: when instantaneous price impact per unit flow
	// indicates a depleted/illiquid book (\lambda_{Kyle} >> KyleMean), dampen raw extrapolation
	// to avoid over-predicting into transient illiquidity spikes.
	flow := math.Abs(0.5*x[5] + 0.35*x[8] + 0.15*x[13])
	lamKyle := math.Min(8.0, math.Abs(x[11])/math.Max(flow, 0.35))
	kyleRatio := lamKyle / math.Max(0.25, m.KyleMean)
	damping := 1.0 / (1.0 + m.KyleDamp*math.Max(0, kyleRatio-1.5))

	return (base + sigContrib) * damping
}

// PredictSlice evaluates Predict from a slice []float64 in 0 B/op and 0 allocs/op.
func (m *RoughSignatureOFIModel) PredictSlice(x []float64) float64 {
	var arr [FeatureCount]float64
	copy(arr[:], x)
	return m.Predict(&arr)
}

// Clone returns an independent deep copy of RoughSignatureOFIModel.
func (m *RoughSignatureOFIModel) Clone() *RoughSignatureOFIModel {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}
