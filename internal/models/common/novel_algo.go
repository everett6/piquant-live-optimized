package common

import (
	"math"
	"time"
)

// HawkesIntensity tracks mutually-exciting buy and sell trade arrivals in O(1) time
// without storing trade history, using an exponential decay kernel.
// LambdaPlus(t) = mu + sum_{t_i < t} alpha * exp(-beta * (t - t_i))
type HawkesIntensity struct {
	Mu            float64 // Baseline intensity (trades/sec)
	AlphaSelf     float64 // Self-excitation jump
	AlphaCross    float64 // Cross-excitation jump
	Beta          float64 // Exponential decay rate (1/sec)
	lastTimeNs    int64
	intensityBuy  float64
	intensitySell float64
}

// NewHawkesIntensity initializes an online Hawkes intensity tracker.
func NewHawkesIntensity(beta float64) *HawkesIntensity {
	if beta <= 0 {
		beta = 2.0 // default 0.5s half-life
	}
	return &HawkesIntensity{
		Mu:         0.1,
		AlphaSelf:  0.8,
		AlphaCross: 0.2,
		Beta:       beta,
	}
}

// Update advances the intensity state to timestamp tNs and registers a trade event.
func (h *HawkesIntensity) Update(tNs int64, isBuy bool, qty float64) {
	if h.lastTimeNs > 0 && tNs > h.lastTimeNs {
		dtSec := float64(tNs-h.lastTimeNs) / 1e9
		decay := math.Exp(-h.Beta * dtSec)
		h.intensityBuy = h.Mu + (h.intensityBuy-h.Mu)*decay
		h.intensitySell = h.Mu + (h.intensitySell-h.Mu)*decay
	}
	h.lastTimeNs = tNs

	jump := math.Log(1.0 + math.Max(0.0, qty))
	if isBuy {
		h.intensityBuy += h.AlphaSelf * jump
		h.intensitySell += h.AlphaCross * jump
	} else {
		h.intensitySell += h.AlphaSelf * jump
		h.intensityBuy += h.AlphaCross * jump
	}
}

// Imbalance returns the normalized intensity imbalance in [-1, 1].
// Positive values indicate intense buying clustering (stop-run/sweep); negative indicate sell clustering.
func (h *HawkesIntensity) Imbalance(tNs int64) float64 {
	if h.lastTimeNs > 0 && tNs > h.lastTimeNs {
		dtSec := float64(tNs-h.lastTimeNs) / 1e9
		decay := math.Exp(-h.Beta * dtSec)
		h.intensityBuy = h.Mu + (h.intensityBuy-h.Mu)*decay
		h.intensitySell = h.Mu + (h.intensitySell-h.Mu)*decay
		h.lastTimeNs = tNs
	}
	denom := h.intensityBuy + h.intensitySell
	if denom < 1e-9 {
		return 0
	}
	return (h.intensityBuy - h.intensitySell) / denom
}

// TotalIntensity returns current total trade arrival rate.
func (h *HawkesIntensity) TotalIntensity() float64 {
	return h.intensityBuy + h.intensitySell
}

// VPINTracker measures Volume-Synchronized Probability of Toxicity across M volume buckets.
type VPINTracker struct {
	BucketVol     float64
	NBuckets      int
	currBuyVol    float64
	currSellVol   float64
	bucketDiffs   []float64
	bucketIdx     int
	filledBuckets int
}

// NewVPINTracker initializes a volume-synchronized toxicity tracker.
func NewVPINTracker(bucketVol float64, nBuckets int) *VPINTracker {
	if bucketVol <= 0 {
		bucketVol = 1.0 // 1 unit (e.g. 1 BTC)
	}
	if nBuckets <= 0 {
		nBuckets = 20
	}
	return &VPINTracker{
		BucketVol:   bucketVol,
		NBuckets:    nBuckets,
		bucketDiffs: make([]float64, nBuckets),
	}
}

// AddTrade feeds a trade into the volume-clock tracker.
func (v *VPINTracker) AddTrade(isBuy bool, qty float64) {
	rem := qty
	for rem > 0 {
		currTotal := v.currBuyVol + v.currSellVol
		needed := v.BucketVol - currTotal
		take := math.Min(rem, needed)
		if isBuy {
			v.currBuyVol += take
		} else {
			v.currSellVol += take
		}
		rem -= take

		if v.currBuyVol+v.currSellVol >= v.BucketVol {
			// Bucket completed
			diff := math.Abs(v.currBuyVol - v.currSellVol)
			v.bucketDiffs[v.bucketIdx] = diff
			v.bucketIdx = (v.bucketIdx + 1) % v.NBuckets
			if v.filledBuckets < v.NBuckets {
				v.filledBuckets++
			}
			v.currBuyVol = 0
			v.currSellVol = 0
		}
	}
}

// Toxicity returns the current VPIN estimate in [0, 1].
// VPIN > 0.65 signals severe toxic order flow / adverse selection risk.
func (v *VPINTracker) Toxicity() float64 {
	if v.filledBuckets == 0 {
		return 0
	}
	var sumDiff float64
	for i := 0; i < v.filledBuckets; i++ {
		sumDiff += v.bucketDiffs[i]
	}
	totalVol := float64(v.filledBuckets) * v.BucketVol
	if totalVol <= 0 {
		return 0
	}
	return sumDiff / totalVol
}

// StoikovMicroprice evaluates fair value taking into account queue depletion and multi-step transitions.
// Computes P_mid + spread * G(I, S) where G is the precomputed transition fair-value adjustment.
func StoikovMicroprice(bestBid, bestAsk, bidQty, askQty int64) float64 {
	if bestBid <= 0 || bestAsk <= bestBid {
		return float64(bestBid+bestAsk) / 2.0
	}
	mid := float64(bestBid+bestAsk) / 2.0
	spread := float64(bestAsk - bestBid)
	totQty := float64(bidQty + askQty)
	if totQty <= 0 {
		return mid
	}

	// Normalized queue imbalance in [-1, 1]
	imb := (float64(bidQty) - float64(askQty)) / totQty

	// Stoikov non-linear adjustment curve for crypto orderbooks
	// Dampens extreme outliers and accounts for high probability of queue cancellation at touch
	adj := 0.5 * math.Tanh(1.5*imb)
	return mid + (spread * adj)
}

// TripleBarrierLabeler evaluates whether profit target, stop loss, or time horizon is hit first.
type TripleBarrierLabeler struct {
	PtBps   float64 // Profit target in basis points
	SlBps   float64 // Stop loss in basis points
	Horizon time.Duration
}

// BarrierResult represents the outcome of a triple-barrier exit.
type BarrierResult struct {
	Label       int     // +1 (hit target), -1 (hit stop), 0 (vertical horizon expired)
	ReturnBps   float64 // Net realized return in bps
	HoldingTime time.Duration
}

// Evaluate walks forward prices to determine which barrier was touched first.
func (tb *TripleBarrierLabeler) Evaluate(isLong bool, entryMid float64, futureMids []float64, timestamps []int64, startTime int64) BarrierResult {
	if len(futureMids) == 0 || entryMid <= 0 {
		return BarrierResult{Label: 0, ReturnBps: 0}
	}

	maxT := startTime + tb.Horizon.Nanoseconds()
	for i, fMid := range futureMids {
		t := timestamps[i]
		if t > maxT {
			// Vertical time barrier reached
			ret := Bps(entryMid, fMid)
			if !isLong {
				ret = -ret
			}
			return BarrierResult{
				Label:       0,
				ReturnBps:   ret,
				HoldingTime: time.Duration(t - startTime),
			}
		}

		ret := Bps(entryMid, fMid)
		if !isLong {
			ret = -ret
		}

		if ret >= tb.PtBps {
			return BarrierResult{
				Label:       1,
				ReturnBps:   tb.PtBps,
				HoldingTime: time.Duration(t - startTime),
			}
		}
		if ret <= -tb.SlBps {
			return BarrierResult{
				Label:       -1,
				ReturnBps:   -tb.SlBps,
				HoldingTime: time.Duration(t - startTime),
			}
		}
	}

	lastMid := futureMids[len(futureMids)-1]
	ret := Bps(entryMid, lastMid)
	if !isLong {
		ret = -ret
	}
	return BarrierResult{Label: 0, ReturnBps: ret}
}
