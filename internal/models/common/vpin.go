package common

import (
	"math"
)

// DefaultVPINBuckets is the standard institutional volume bucket buffer size.
const DefaultVPINBuckets = 50

// VPINCalculator computes the Volume-Synchronized Probability of Toxicity (VPIN)
// using a stack/pre-allocated fixed circular buffer of N volume buckets.
//
// Layout is 64-byte cache-line aligned:
//   - Cache Line 0 (bytes 0..63): hot scalar state (BucketVol, CurrBuy, CurrSell, NumBuckets, HeadIdx, Filled).
//   - Cache Lines 1..7 (bytes 64..463): circular buffer Diffs begins at exactly offset 64,
//     preventing split cache-line penalties and enabling aligned vector loads on Cortex-A76.
//
// All methods on VPINCalculator execute in 0 B/op and 0 allocs/op.
type VPINCalculator struct {
	BucketVol  float64                     // Volume per bucket (V)
	CurrBuy    float64                     // Current accumulating buy volume in open bucket
	CurrSell   float64                     // Current accumulating sell volume in open bucket
	NumBuckets int                         // Total volume buckets (N, up to 50)
	HeadIdx    int                         // Circular buffer head
	Filled     int                         // Number of completed buckets in buffer
	_          [16]byte                    // 16 bytes pad to 64 bytes (Cache Line 0 boundary)
	Diffs      [DefaultVPINBuckets]float64 // Absolute order imbalance |V_tau^B - V_tau^S| (offset 64)
}

// NewVPINCalculator initializes a VPINCalculator with bucket volume V and capacity N.
// If n <= 0 or n > DefaultVPINBuckets, DefaultVPINBuckets (50) is used.
func NewVPINCalculator(bucketVol float64, n int) *VPINCalculator {
	if bucketVol <= 0 {
		bucketVol = 1.0
	}
	if n <= 0 || n > DefaultVPINBuckets {
		n = DefaultVPINBuckets
	}
	return &VPINCalculator{
		BucketVol:  bucketVol,
		NumBuckets: n,
	}
}

// Update processes a trade print with tradePrice, tradeSize, and aggressor side indicator
// (isBuyerMaker: true if the buyer was the maker, meaning the aggressor was a seller;
// false if the aggressor was a buyer).
//
// Slices volume across bucket boundaries when tradeSize exceeds remaining bucket capacity.
// Operates on the hot path in 0 B/op, 0 allocs/op.
func (v *VPINCalculator) Update(tradePrice, tradeSize float64, isBuyerMaker bool) {
	if tradeSize <= 0 {
		return
	}
	bVol := v.BucketVol
	if bVol <= 0 {
		bVol = 1.0
		v.BucketVol = bVol
	}
	n := v.NumBuckets
	if n <= 0 || n > DefaultVPINBuckets {
		n = DefaultVPINBuckets
		v.NumBuckets = n
	}

	// isBuyerMaker == true means the maker was buyer, so the incoming trade was an aggressive SELL.
	// isBuyerMaker == false means aggressive BUY.
	isBuy := !isBuyerMaker

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
		}
	}
}

// VPIN returns the current Volume-Synchronized Probability of Toxicity:
// VPIN = (1 / (N * V)) * \sum_{tau=1}^N |V_tau^B - V_tau^S|
// Returns 0 if no buckets have been filled.
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

// IsToxic reports whether current VPIN exceeds threshold.
func (v *VPINCalculator) IsToxic(threshold float64) bool {
	return v.VPIN() >= threshold
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
// If the effective depletion drift is positive (queue is draining), the first passage
// probability is computed analytically:
//
//	Depletion rate: delta = mu_trade + lambda_cancel + max(0, -OFI)
//	P(Fill) = 1 - exp(- (delta * horizonSec) / max(q_ahead, epsilon))
func (c ContDeLarrardQueue) FillProbability(horizonSec float64) float64 {
	if horizonSec <= 0 {
		return 0
	}
	if c.QueueAhead <= 1e-9 {
		return 1.0
	}

	// Net depletion rate per second
	// Trade arrival and cancellations always deplete the queue ahead.
	// Adverse OFI (negative to our queue) further accelerates queue depletion.
	ofiDrain := 0.0
	if c.OFI < 0 {
		ofiDrain = -c.OFI
	}
	depletionRate := c.TradeRate + c.CancelRate + ofiDrain
	if depletionRate <= 1e-9 {
		return 0
	}

	// Characteristic queue depletion scale
	lambdaEff := depletionRate / c.QueueAhead
	// P(T <= horizonSec) for Poisson/exponential first-passage approximation
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
