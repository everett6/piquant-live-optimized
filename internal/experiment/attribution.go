package experiment

import (
	"math"

	"piquant-live/internal/models/common"
)

// ComputeMarkoutAttribution decomposes gross and net trade performance across a chronological
// sample slice into four institutional execution & uncertainty components:
//   - spreadCaptureBps: edge earned from half-spread capture on passive/quasi-passive entries
//     weighted by FIFO queue-priority fill probability (depleted by trade arrival & OFI).
//   - directionalAlphaBps: medium-horizon fundamental drift contribution beyond immediate tick impact.
//   - markoutToxicityBps: immediate 1-tick adverse selection loss from toxic sweeps against the entry.
//   - conformalPassRate: fraction of active signals in [0, 1] whose 90% split-conformal uncertainty
//     bound clears the transaction cost hurdle.
func ComputeMarkoutAttribution(samples []common.Sample, preds []float64, threshold float64, costBps float64) (spreadCaptureBps, directionalAlphaBps, markoutToxicityBps, conformalPassRate float64) {
	n := len(samples)
	if n == 0 || len(preds) < n {
		return 0, 0, 0, 0
	}
	if threshold < 0 {
		threshold = 0
	}
	if costBps < 0 {
		costBps = 0
	}

	// Calibrate 90% Split Conformal Calibrator (\alpha = 0.10) on the first half of the slice
	// (or full slice when compact) to evaluate distribution-free uncertainty gating.
	cal := common.NewSplitConformalCalibrator(0.10)
	calEnd := n / 2
	if calEnd < 16 {
		calEnd = n
	}
	cal.Calibrate(samples[:calEnd], preds[:calEnd])

	var sumSpreadCap, sumDirAlpha, sumToxicity float64
	var activeSignals, conformalPassed int

	for i := 0; i < n; i++ {
		p := preds[i]
		if math.Abs(p) <= threshold {
			continue
		}
		activeSignals++

		s := &samples[i]
		dir := 1.0
		if p < 0 {
			dir = -1.0
		}

		// Local volatility from sample or feature x[10] (vol_30s_bps)
		localVol := s.Vol
		if localVol <= 0 {
			localVol = math.Max(0.25, math.Abs(s.X[10]))
		}
		if cal.PassesConformalGate(p, localVol, costBps) {
			conformalPassed++
		}

		// FIFO Queue-Priority Passive Fill Simulator:
		// Spread in bps (x[4] or s.Spread), queue_ratio (x[15]), trade_cnt_1s (x[14]), ofi_1s (x[5]).
		spread := s.Spread
		if spread <= 0 {
			spread = s.X[4]
		}
		spread = math.Max(0.20, spread)
		halfSpread := 0.5 * spread

		qRatio := s.X[15] // in [-1, 1] or [0, 1]
		queueAhead := 0.5 + 0.35*dir*qRatio
		if queueAhead < 0.15 {
			queueAhead = 0.15
		} else if queueAhead > 0.90 {
			queueAhead = 0.90
		}

		tradeArrival := math.Max(0.5, s.X[14])
		ofiDepletion := math.Abs(s.X[5])*0.15 + math.Abs(s.X[8])*0.20
		fifoFillProb := 1.0 - math.Exp(-(0.12*tradeArrival+ofiDepletion)/queueAhead)
		if fifoFillProb < 0.15 {
			fifoFillProb = 0.15
		} else if fifoFillProb > 0.95 {
			fifoFillProb = 0.95
		}

		// 1. SpreadCaptureBps: passive/quasi-passive half-spread capture net of fee drag
		spreadCap := halfSpread*fifoFillProb - costBps*(1.0-0.5*fifoFillProb)
		if spreadCap < 0.05*halfSpread {
			spreadCap = 0.05 * halfSpread
		}

		// 2. MarkoutToxicityBps: immediate 1-tick adverse selection loss from toxic sweeps
		var immediateAdverse float64
		if i+1 < n {
			// Immediate 1-step price move via mid or forward return difference
			var stepMoveBps float64
			if s.Mid > 0 && samples[i+1].Mid > 0 {
				stepMoveBps = common.Bps(s.Mid, samples[i+1].Mid)
			} else {
				stepMoveBps = (s.Y - samples[i+1].Y) * 0.25
			}
			if dir*stepMoveBps < 0 {
				immediateAdverse = math.Abs(dir * stepMoveBps)
			}
		}
		// Combine observed 1-tick adverse markout with microstructural sweep toxicity
		sweepToxicity := 0.06*math.Abs(s.X[13]) + 0.04*math.Abs(s.X[8]) + 0.03*localVol
		toxLoss := 0.6*immediateAdverse + 0.4*sweepToxicity
		if toxLoss < 0.02 {
			toxLoss = 0.02
		}

		// 3. DirectionalAlphaBps: medium-horizon fundamental drift contribution
		realizedEdge := dir * s.Y
		dirAlpha := realizedEdge - spreadCap + toxLoss

		sumSpreadCap += spreadCap
		sumDirAlpha += dirAlpha
		sumToxicity += toxLoss
	}

	if activeSignals == 0 {
		return 0, 0, 0, 0
	}

	inv := 1.0 / float64(activeSignals)
	spreadCaptureBps = sumSpreadCap * inv
	directionalAlphaBps = sumDirAlpha * inv
	markoutToxicityBps = sumToxicity * inv
	conformalPassRate = float64(conformalPassed) / float64(activeSignals)
	return spreadCaptureBps, directionalAlphaBps, markoutToxicityBps, conformalPassRate
}
