package experiment

import (
	"math"
	"path/filepath"
	"strings"
)

// KalmanCointegrationTracker estimates the time-varying hedge ratio beta_t
// between two co-moving crypto assets (e.g., BTC and ETH) in O(1) time per observation.
type KalmanCointegrationTracker struct {
	Beta        float64 // State estimate: dynamic hedge ratio
	P           float64 // State covariance
	Q           float64 // Process noise variance
	R           float64 // Measurement noise variance
	ewmaMean    float64
	ewmaVar     float64
	alpha       float64
	initialized bool
}

// NewKalmanCointegrationTracker creates a 1D Kalman filter for statistical arbitrage tracking.
func NewKalmanCointegrationTracker(q, r float64) *KalmanCointegrationTracker {
	if q <= 0 {
		q = 1e-5
	}
	if r <= 0 {
		r = 1e-3
	}
	return &KalmanCointegrationTracker{
		Beta:  1.0,
		P:     1.0,
		Q:     q,
		R:     r,
		alpha: 0.05,
	}
}

// Update ingests synchronized normalized returns or log-price ratios (x = leader BTC, y = follower ETH)
// and returns the current cointegration spread z-score and updated hedge beta.
func (k *KalmanCointegrationTracker) Update(xLeader, yFollower float64) (zScore float64, beta float64) {
	if !k.initialized {
		if math.Abs(xLeader) > 1e-9 {
			k.Beta = yFollower / xLeader
		} else {
			k.Beta = 1.0
		}
		k.P = 0.1
		k.ewmaVar = 1.0
		k.initialized = true
		return 0, k.Beta
	}

	// Prediction step
	pPred := k.P + k.Q

	// Innovation (spread residual)
	spread := yFollower - k.Beta*xLeader
	sVar := xLeader*xLeader*pPred + k.R
	if sVar < 1e-12 {
		sVar = 1e-12
	}

	// Kalman gain & state update
	kGain := (pPred * xLeader) / sVar
	k.Beta = k.Beta + kGain*spread
	k.P = (1.0 - kGain*xLeader) * pPred

	// EWMA z-score normalization of the residual spread
	diff := spread - k.ewmaMean
	k.ewmaMean += k.alpha * diff
	k.ewmaVar = (1.0 - k.alpha) * (k.ewmaVar + k.alpha*diff*diff)
	std := math.Sqrt(math.Max(1e-9, k.ewmaVar))
	zScore = (spread - k.ewmaMean) / std
	if zScore > 6.0 {
		zScore = 6.0
	} else if zScore < -6.0 {
		zScore = -6.0
	}
	return zScore, k.Beta
}

// CrossAssetStats summarizes lead-lag cross-impact (BTC -> ETH) and statistical arbitrage metrics.
type CrossAssetStats struct {
	LeaderSymbol    string  `json:"leader_symbol"`
	FollowerSymbol  string  `json:"follower_symbol"`
	KalmanBeta      float64 `json:"kalman_beta"`       // Dynamic hedge ratio beta_{BTC->ETH}
	CrossFlowCorr   float64 `json:"cross_flow_corr"`   // Correlation(MLOFI_BTC, Ret_ETH)
	SpreadZScore    float64 `json:"spread_z_score"`    // Latest cointegration residual z-score
	HalfLifeSamples float64 `json:"half_life_samples"` // Mean-reversion half-life in observations
}

// EvaluateCrossAssetLeadLag computes Kalman cointegration and cross-asset order flow lead-lag
// between two sample slices (leader e.g. BTCUSDT, follower e.g. ETHUSDT).
func EvaluateCrossAssetLeadLag(leader, follower []FeatureObservation) CrossAssetStats {
	n := len(leader)
	if len(follower) < n {
		n = len(follower)
	}
	if n < 10 {
		return CrossAssetStats{
			LeaderSymbol:   "BTCUSDT",
			FollowerSymbol: "ETHUSDT",
			KalmanBeta:     1.0,
		}
	}

	kf := NewKalmanCointegrationTracker(1e-5, 1e-3)
	var lastZ, lastBeta float64
	var sumXY, sumX2, sumY2 float64
	spreads := make([]float64, n)

	baseLeader := leader[0].MidPrice
	baseFollower := follower[0].MidPrice
	if baseLeader <= 0 {
		baseLeader = 1.0
	}
	if baseFollower <= 0 {
		baseFollower = 1.0
	}

	for i := 0; i < n; i++ {
		// Cumulative return in bps from start of window
		rLeader := ((leader[i].MidPrice - baseLeader) / baseLeader) * 1e4
		rFollower := ((follower[i].MidPrice - baseFollower) / baseFollower) * 1e4
		lastZ, lastBeta = kf.Update(rLeader, rFollower)
		spreads[i] = rFollower - lastBeta*rLeader

		// Cross-impact: Leader MLOFI_1s (index 8) leading Follower 1s return (index 11)
		if i+1 < n {
			leaderOFI := leader[i].Features[8]
			followerNextRet := follower[i+1].Features[11]
			sumXY += leaderOFI * followerNextRet
			sumX2 += leaderOFI * leaderOFI
			sumY2 += followerNextRet * followerNextRet
		}
	}

	var crossCorr float64
	denom := math.Sqrt(sumX2 * sumY2)
	if denom > 1e-9 {
		crossCorr = sumXY / denom
	}

	// Estimate Ornstein-Uhlenbeck AR(1) mean-reversion half-life of the cointegration spread:
	// delta_s_t = phi * s_{t-1} + eps_t  => half_life = -ln(2) / ln(1 + phi)
	var numPhi, denPhi float64
	for i := 1; i < n; i++ {
		ds := spreads[i] - spreads[i-1]
		sPrev := spreads[i-1]
		numPhi += ds * sPrev
		denPhi += sPrev * sPrev
	}
	halfLife := 15.0
	if denPhi > 1e-9 {
		phi := numPhi / denPhi
		if phi < -0.001 && phi > -0.99 {
			hl := -math.Ln2 / math.Log(1.0+phi)
			if hl > 1.0 && hl < 500.0 {
				halfLife = hl
			}
		}
	}

	return CrossAssetStats{
		LeaderSymbol:    "BTCUSDT",
		FollowerSymbol:  "ETHUSDT",
		KalmanBeta:      lastBeta,
		CrossFlowCorr:   crossCorr,
		SpreadZScore:    lastZ,
		HalfLifeSamples: halfLife,
	}
}

// FindLeaderDataset checks if datasetPath is an altcoin/ETH capture and locates a companion BTC capture
// in the same directory for multi-asset lead-lag enrichment.
func FindLeaderDataset(datasetPath string) string {
	base := strings.ToLower(filepath.Base(datasetPath))
	if !strings.Contains(base, "eth") {
		return ""
	}
	dir := filepath.Dir(datasetPath)
	candidate := filepath.Join(dir, strings.Replace(base, "eth", "btc", 1))
	return candidate
}
