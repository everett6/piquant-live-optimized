package common

import (
	"math"
)

// DefaultVPINBuckets is the standard institutional volume bucket buffer size.
const DefaultVPINBuckets = 50

// VPINCalculator computes the Volume-Synchronized Probability of Toxicity (VPIN)
// using a stack/pre-allocated fixed circular buffer of N volume buckets (Easley, López de Prado, O'Hara 2012).
//
// Layout is 64-byte cache-line aligned:
//   - Cache Line 0 (bytes 0..63): hot scalar state.
//   - Cache Lines 1..7 (bytes 64..463): circular buffer Diffs begins at exactly offset 64,
//     preventing split cache-line penalties and enabling aligned vector loads on Cortex-A76.
//
// All methods on VPINCalculator execute in 0 B/op and 0 allocs/op.
type VPINCalculator struct {
	BucketVol      float64                     // Volume per bucket (V)
	CurrBuy        float64                     // Current accumulating buy volume in open bucket
	CurrSell       float64                     // Current accumulating sell volume in open bucket
	NumBuckets     int                         // Total volume buckets (N, up to 50)
	HeadIdx        int                         // Circular buffer head
	Filled         int                         // Number of completed buckets in buffer
	ToxicThreshold float64                     // Threshold for toxicity classification (default 0.70)
	BasePenaltyBps float64                     // Base quote widening penalty in bps (default 5.0)
	Diffs          [DefaultVPINBuckets]float64 // Absolute order imbalance |V_tau^B - V_tau^S| (offset 64)

	// Price and historical tracking for tick rule, BVC, and empirical CDF
	LastPrice   float64
	LastTickBuy bool
	HistHead    int
	HistCount   int
	MeanVPIN    float64
	VarVPIN     float64
	HistVPIN    [100]float64 // Circular history buffer for empirical CDF estimation
}

// NewVPINCalculator initializes a VPINCalculator with bucket volume V and capacity N.
// If n <= 0 or n > DefaultVPINBuckets, DefaultVPINBuckets (50) is used.
func NewVPINCalculator(bucketVol float64, n int) *VPINCalculator {
	if bucketVol <= 0 {
		bucketVol = 5.0
	}
	if n <= 0 || n > DefaultVPINBuckets {
		n = DefaultVPINBuckets
	}
	return &VPINCalculator{
		BucketVol:      bucketVol,
		NumBuckets:     n,
		ToxicThreshold: 0.70,
		BasePenaltyBps: 5.0,
	}
}

// UpdateTrade processes a trade print with tradePrice, tradeSize, and aggressor side indicator
// (isBuyerMaker: true if the buyer was the maker, meaning the aggressor was a seller;
// false if the aggressor was a buyer).
//
// Slices volume across bucket boundaries when tradeSize exceeds remaining bucket capacity.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (v *VPINCalculator) UpdateTrade(tradePrice, tradeSize float64, isBuyerMaker bool) {
	if tradeSize <= 0 {
		return
	}
	v.LastPrice = tradePrice
	isBuy := !isBuyerMaker
	v.LastTickBuy = isBuy
	v.accumulateSingle(tradeSize, isBuy)
}

// Update provides backwards compatibility for existing callers of Update.
func (v *VPINCalculator) Update(tradePrice, tradeSize float64, isBuyerMaker bool) {
	v.UpdateTrade(tradePrice, tradeSize, isBuyerMaker)
}

// UpdateTradeTick classifies incoming trade volume using the exchange tick rule:
// upticks are classified as buys, downticks as sells, and zero-ticks inherit the prior direction.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (v *VPINCalculator) UpdateTradeTick(tradePrice, tradeSize float64) {
	if tradePrice <= 0 {
		return
	}
	if v.LastPrice <= 0 {
		v.LastPrice = tradePrice
		v.LastTickBuy = true
		if tradeSize <= 0 {
			return
		}
	}
	if tradeSize <= 0 {
		v.LastPrice = tradePrice
		return
	}
	isBuy := v.LastTickBuy
	if tradePrice > v.LastPrice {
		isBuy = true
		v.LastTickBuy = true
	} else if tradePrice < v.LastPrice {
		isBuy = false
		v.LastTickBuy = false
	}
	v.LastPrice = tradePrice
	v.accumulateSingle(tradeSize, isBuy)
}

// UpdateTradeBVC partitions trade volume using Bulk Volume Classification (BVC):
// V_tau^B = tradeSize * Phi(Delta P / sigma), V_tau^S = tradeSize * (1 - Phi(Delta P / sigma)).
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (v *VPINCalculator) UpdateTradeBVC(tradePrice, tradeSize, sigmaPrice float64) {
	if tradePrice <= 0 {
		return
	}
	if v.LastPrice <= 0 {
		v.LastPrice = tradePrice
		if tradeSize <= 0 {
			return
		}
	}
	if tradeSize <= 0 {
		v.LastPrice = tradePrice
		return
	}
	if sigmaPrice <= 1e-9 {
		sigmaPrice = 1.0
	}
	deltaP := tradePrice - v.LastPrice
	v.LastPrice = tradePrice

	// Standard normal CDF: Phi(z) = 0.5 * (1 + erf(z / sqrt(2)))
	z := deltaP / (sigmaPrice * math.Sqrt2)
	buyFrac := 0.5 * (1.0 + math.Erf(z))
	if buyFrac < 0.0 {
		buyFrac = 0.0
	} else if buyFrac > 1.0 {
		buyFrac = 1.0
	}
	buyVol := tradeSize * buyFrac
	sellVol := tradeSize * (1.0 - buyFrac)
	v.accumulateSplit(buyVol, sellVol)
}

// accumulateSingle accumulates trade volume where all volume belongs to a single side.
func (v *VPINCalculator) accumulateSingle(tradeSize float64, isBuy bool) {
	bVol := v.BucketVol
	if bVol <= 0 {
		bVol = 5.0
		v.BucketVol = bVol
	}
	n := v.NumBuckets
	if n <= 0 || n > DefaultVPINBuckets {
		n = DefaultVPINBuckets
		v.NumBuckets = n
	}

	rem := tradeSize
	for rem > 0 {
		currTot := v.CurrBuy + v.CurrSell
		space := bVol - currTot
		take := rem
		if take > space {
			take = space
		}

		if isBuy {
			v.CurrBuy += take
		} else {
			v.CurrSell += take
		}
		rem -= take

		// Bucket completion check
		if v.CurrBuy+v.CurrSell >= bVol {
			diff := v.CurrBuy - v.CurrSell
			if diff < 0 {
				diff = -diff
			}
			v.Diffs[v.HeadIdx] = diff
			v.HeadIdx = (v.HeadIdx + 1) % n
			if v.Filled < n {
				v.Filled++
			}
			v.CurrBuy = 0
			v.CurrSell = 0
			v.recordBucketVPIN()
		}
	}
}

// accumulateSplit accumulates trade volume partitioned across buy and sell.
func (v *VPINCalculator) accumulateSplit(buyVol, sellVol float64) {
	tot := buyVol + sellVol
	if tot <= 0 {
		return
	}
	bVol := v.BucketVol
	if bVol <= 0 {
		bVol = 5.0
		v.BucketVol = bVol
	}
	n := v.NumBuckets
	if n <= 0 || n > DefaultVPINBuckets {
		n = DefaultVPINBuckets
		v.NumBuckets = n
	}

	buyRatio := buyVol / tot
	sellRatio := sellVol / tot
	rem := tot

	for rem > 0 {
		currTot := v.CurrBuy + v.CurrSell
		space := bVol - currTot
		take := rem
		if take > space {
			take = space
		}

		v.CurrBuy += take * buyRatio
		v.CurrSell += take * sellRatio
		rem -= take

		// Bucket completion check
		if v.CurrBuy+v.CurrSell >= bVol {
			diff := v.CurrBuy - v.CurrSell
			if diff < 0 {
				diff = -diff
			}
			v.Diffs[v.HeadIdx] = diff
			v.HeadIdx = (v.HeadIdx + 1) % n
			if v.Filled < n {
				v.Filled++
			}
			v.CurrBuy = 0
			v.CurrSell = 0
			v.recordBucketVPIN()
		}
	}
}

// recordBucketVPIN updates the rolling historical VPIN distribution for empirical CDF tracking.
func (v *VPINCalculator) recordBucketVPIN() {
	curr := v.VPIN()
	v.HistVPIN[v.HistHead] = curr
	v.HistHead = (v.HistHead + 1) % len(v.HistVPIN)
	if v.HistCount < len(v.HistVPIN) {
		v.HistCount++
	}

	if v.HistCount == 1 {
		v.MeanVPIN = curr
		v.VarVPIN = 0.01
	} else {
		diff := curr - v.MeanVPIN
		v.MeanVPIN += 0.05 * diff
		diffPost := curr - v.MeanVPIN
		v.VarVPIN = 0.95*v.VarVPIN + 0.05*(diff*diffPost)
	}
}

// VPIN returns the current Volume-Synchronized Probability of Toxicity:
//
//	VPIN = (1 / (N * V)) * \sum_{tau=1}^N |V_tau^B - V_tau^S|
//
// Returns 0 if no buckets have been filled.
// Hot path executes in 0 B/op and 0 allocs/op.
func (v *VPINCalculator) VPIN() float64 {
	if v.Filled == 0 {
		return 0
	}
	var sumDiff float64
	// Unroll 4-lane accumulation to prevent serial addition dependency stalls on Cortex-A76
	i := 0
	for ; i+4 <= v.Filled; i += 4 {
		sumDiff += (v.Diffs[i] + v.Diffs[i+1]) + (v.Diffs[i+2] + v.Diffs[i+3])
	}
	for ; i < v.Filled; i++ {
		sumDiff += v.Diffs[i]
	}
	denom := float64(v.Filled) * v.BucketVol
	if denom <= 0 {
		return 0
	}
	return sumDiff / denom
}

// CDF returns the empirical cumulative distribution function percentile P(VPIN_hist <= current_VPIN)
// over the rolling historical VPIN window, bounded in [0, 1].
// Hot path executes in 0 B/op and 0 allocs/op.
func (v *VPINCalculator) CDF() float64 {
	if v.Filled == 0 || v.HistCount == 0 {
		return 0.5
	}
	curr := v.VPIN()
	count := 0
	for i := 0; i < v.HistCount; i++ {
		if v.HistVPIN[i] <= curr {
			count++
		}
	}
	cdf := float64(count) / float64(v.HistCount)
	if cdf < 0 {
		return 0
	}
	if cdf > 1.0 {
		return 1.0
	}
	return cdf
}

// IsToxic reports whether current VPIN exceeds the toxicity threshold.
// If an explicit threshold is provided in args, that value is used; otherwise v.ToxicThreshold (0.70 default) is used.
// Hot path executes in 0 B/op and 0 allocs/op.
func (v *VPINCalculator) IsToxic(threshold ...float64) bool {
	th := v.ToxicThreshold
	if th <= 0 {
		th = 0.70
	}
	if len(threshold) > 0 && threshold[0] > 0 {
		th = threshold[0]
	}
	return v.VPIN() >= th
}

// AdjustQuote widens maker quotes when toxic order flow is detected to avert adverse selection.
// When VPIN >= ToxicThreshold, quotes are widened symmetrically by a penalty in basis points.
// Hot path executes in 0 B/op and 0 allocs/op.
func (v *VPINCalculator) AdjustQuote(bidPrice, askPrice float64) (newBid, newAsk float64, widenedBps float64) {
	if bidPrice <= 0 || askPrice <= 0 {
		return bidPrice, askPrice, 0.0
	}
	vpin := v.VPIN()
	th := v.ToxicThreshold
	if th <= 0 {
		th = 0.70
	}
	if vpin < th {
		return bidPrice, askPrice, 0.0
	}

	excess := (vpin - th) / math.Max(0.01, 1.0-th)
	basePenalty := v.BasePenaltyBps
	if basePenalty <= 0 {
		basePenalty = 5.0
	}
	// Widening penalty scales up to basePenalty + 20 bps
	widenedBps = basePenalty + excess*15.0
	halfWiden := (bidPrice * (widenedBps / 10000.0)) / 2.0

	newBid = bidPrice - halfWiden
	newAsk = askPrice + halfWiden
	return newBid, newAsk, widenedBps
}

// Reset clears the VPIN calculator state for reuse.
func (v *VPINCalculator) Reset() {
	v.CurrBuy = 0
	v.CurrSell = 0
	v.HeadIdx = 0
	v.Filled = 0
	for i := range v.Diffs {
		v.Diffs[i] = 0
	}
	v.HistHead = 0
	v.HistCount = 0
	v.MeanVPIN = 0
	v.VarVPIN = 0
	v.LastPrice = 0
	v.LastTickBuy = false
}

// VPINToxicityModel implements ContinuousModel for research sweeps, ablation gates, and CV.
type VPINToxicityModel struct {
	BaseRidge Ridge
	Threshold float64
	fitted    bool
}

// NewVPINToxicityModel constructs an unfitted VPINToxicityModel.
func NewVPINToxicityModel(lambda, threshold float64) *VPINToxicityModel {
	if lambda <= 0 {
		lambda = 1e-3
	}
	if threshold <= 0 {
		threshold = 0.70
	}
	return &VPINToxicityModel{
		BaseRidge: Ridge{Lam: lambda},
		Threshold: threshold,
	}
}

// Fit trains the base Ridge weights on the training samples.
func (m *VPINToxicityModel) Fit(samples []Sample, idx []int) error {
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
	m.fitted = true
	return nil
}

// Predict calculates forward alpha forecast in bps from the 16-element core feature vector,
// applying adverse selection dampening under toxic order flow conditions.
// Hot path executes in 0 B/op and 0 allocs/op.
func (m *VPINToxicityModel) Predict(x *[FeatureCount]float64) float64 {
	baseAlpha := 0.0
	if m.fitted {
		baseAlpha = m.BaseRidge.Predict(x)
	}

	// Microstructural toxicity synthesis from trade imbalance, frequency, and spread
	imb := math.Abs(x[13])                                    // trade_imb_5s
	intensity := math.Min(1.0, math.Max(0.0, x[14]/25.0))     // trade_cnt_1s
	spreadExp := math.Min(1.0, math.Max(0.0, (x[4]-1.0)/4.0)) // spread_bps

	synToxicity := 0.50*imb + 0.30*intensity + 0.20*spreadExp
	if synToxicity > 1.0 {
		synToxicity = 1.0
	}

	damping := 1.0
	if synToxicity >= m.Threshold {
		excess := (synToxicity - m.Threshold) / math.Max(0.01, 1.0-m.Threshold)
		damping = math.Max(0.15, 1.0-excess*0.75)
	}

	return baseAlpha * damping
}

// ContDeLarrardQueue models limit order queue position and survival probability
// using a Markovian order book queue depletion framework (Cont & de Larrard).
//
// Methods execute in 0 B/op and 0 allocs/op.
type ContDeLarrardQueue struct {
	QueueAhead float64 // Distance / volume ahead in queue (q_ahead)
	CancelRate float64 // Queue cancellation intensity lambda_cancel (per second)
	TradeRate  float64 // Trade arrival intensity mu_trade (per second)
	OFI        float64 // Order Flow Imbalance draining/replenishing queue
}

// NewContDeLarrardQueue creates a new Cont–de Larrard queue model instance.
func NewContDeLarrardQueue(qAhead, cancelRate, tradeRate, ofi float64) ContDeLarrardQueue {
	return ContDeLarrardQueue{
		QueueAhead: qAhead,
		CancelRate: cancelRate,
		TradeRate:  tradeRate,
		OFI:        ofi,
	}
}

// FillProbability computes the probability of queue exhaustion (order fill) within horizonSec.
func (c ContDeLarrardQueue) FillProbability(horizonSec float64) float64 {
	if horizonSec <= 0 {
		return 0
	}
	if c.QueueAhead <= 1e-9 {
		return 1.0
	}

	ofiDrain := 0.0
	if c.OFI < 0 {
		ofiDrain = -c.OFI
	}
	depletionRate := c.TradeRate + c.CancelRate + ofiDrain
	if depletionRate <= 1e-9 {
		return 0
	}

	lambdaEff := depletionRate / c.QueueAhead
	p := 1.0 - math.Exp(-lambdaEff*horizonSec)
	if p < 0 {
		return 0
	}
	if p > 1.0 {
		return 1.0
	}
	return p
}

// ExpectedTimeToFill computes the expected time until our order reaches the front
// of the queue and fills. Returns math.Inf(1) if depletion rate is non-positive.
func (c ContDeLarrardQueue) ExpectedTimeToFill() float64 {
	if c.QueueAhead <= 1e-9 {
		return 0
	}
	ofiDrain := 0.0
	if c.OFI < 0 {
		ofiDrain = -c.OFI
	}
	depletionRate := c.TradeRate + c.CancelRate + ofiDrain
	if depletionRate <= 1e-9 {
		return math.Inf(1)
	}
	return c.QueueAhead / depletionRate
}
