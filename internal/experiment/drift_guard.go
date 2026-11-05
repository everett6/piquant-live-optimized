package experiment

import "math"

// DriftGuardStatus reports the live sequential CUSUM / Page–Hinkley change-point
// drift monitor state and auto-rollback status for the Paper Trading Blotter.
type DriftGuardStatus struct {
	DriftDetected   bool    `json:"drift_detected"`
	Statistic       float64 `json:"statistic"`
	Threshold       float64 `json:"threshold"`
	MeanResidualBps float64 `json:"mean_residual_bps"`
	MeanFeatureNorm float64 `json:"mean_feature_norm"`
	RollbackCount   int64   `json:"rollback_count"`
	Samples         int64   `json:"samples"`
}

// PageHinkleyDriftGuard monitors rolling prediction residuals and feature norm shift
// (mean/variance drift) using the sequential Page–Hinkley / CUSUM change-point statistic:
//
//	m_t = \sum_{s=1}^t (z_s - \bar{z}_t - \delta), \quad M_t = \min_{1 \le s \le t} m_s
//	PH_t = m_t - M_t > \lambda
//
// Update executes in O(1) time with 0 B/op and 0 allocs/op.
type PageHinkleyDriftGuard struct {
	Delta          float64 `json:"delta"`         // Minimum magnitude of tolerable drift \delta > 0
	Lambda         float64 `json:"lambda"`        // Detection threshold \lambda > 0
	Alpha          float64 `json:"alpha"`         // Exponential forgetting factor for baseline mean/variance
	MinWarmup      int64   `json:"min_warmup"`    // Minimum observations before triggering drift alarm
	AutoRollback   bool    `json:"auto_rollback"` // Reset cumulative statistic automatically after rollback
	meanRes        float64
	varRes         float64
	meanFeat       float64
	varFeat        float64
	cumSum         float64
	minCumSum      float64
	lastStat       float64
	driftTriggered bool
	rollbackCount  int64
	count          int64
}

// NewPageHinkleyDriftGuard constructs a Page–Hinkley sequential change-point drift monitor.
func NewPageHinkleyDriftGuard(delta, threshold float64) *PageHinkleyDriftGuard {
	if delta <= 0 {
		delta = 0.15
	}
	if threshold <= 0 {
		threshold = 12.0
	}
	return &PageHinkleyDriftGuard{
		Delta:        delta,
		Lambda:       threshold,
		Alpha:        0.02,
		MinWarmup:    15,
		AutoRollback: true,
		varRes:       1.0,
		varFeat:      1.0,
	}
}

// Update ingests a prediction residual magnitude (in bps) and feature vector norm,
// updates the sequential Page–Hinkley statistic in O(1) time (0 B/op, 0 allocs/op),
// and returns (driftDetected, statistic, threshold).
func (g *PageHinkleyDriftGuard) Update(residualBps, featureNorm float64) (driftDetected bool, statistic, threshold float64) {
	delta := g.Delta
	if delta <= 0 {
		delta = 0.15
	}
	lam := g.Lambda
	if lam <= 0 {
		lam = 12.0
	}
	alpha := g.Alpha
	if alpha <= 0 || alpha >= 1.0 {
		alpha = 0.02
	}
	warmup := g.MinWarmup
	if warmup <= 0 {
		warmup = 15
	}

	absRes := math.Abs(residualBps)
	absFeat := math.Abs(featureNorm)
	g.count++

	if g.count == 1 {
		g.meanRes = absRes
		g.varRes = math.Max(0.25, absRes*absRes*0.25)
		g.meanFeat = absFeat
		g.varFeat = math.Max(0.25, absFeat*absFeat*0.25)
		g.cumSum = 0
		g.minCumSum = 0
		g.lastStat = 0
		g.driftTriggered = false
		return false, 0, lam
	}

	// Standardized innovations before updating baseline when inside potential excursion
	stdRes := math.Sqrt(math.Max(0.10, g.varRes))
	stdFeat := math.Sqrt(math.Max(0.10, g.varFeat))

	zRes := (absRes - g.meanRes) / stdRes
	zFeat := math.Abs(absFeat-g.meanFeat) / stdFeat

	// Combined residual + feature shift score
	jointShift := 0.70*zRes + 0.30*zFeat

	// Update running EWMA baseline slowly so abrupt regime jumps accumulate in Page-Hinkley
	diffRes := absRes - g.meanRes
	g.meanRes += alpha * diffRes
	g.varRes = (1.0-alpha)*g.varRes + alpha*(diffRes*diffRes)

	diffFeat := absFeat - g.meanFeat
	g.meanFeat += alpha * diffFeat
	g.varFeat = (1.0-alpha)*g.varFeat + alpha*(diffFeat*diffFeat)

	// Page-Hinkley sequential accumulation: m_t = m_{t-1} + (jointShift - delta)
	g.cumSum += jointShift - delta
	if g.cumSum < g.minCumSum {
		g.minCumSum = g.cumSum
	}

	statistic = g.cumSum - g.minCumSum
	g.lastStat = statistic

	if g.count >= warmup && statistic > lam {
		driftDetected = true
		g.driftTriggered = true
		g.rollbackCount++
		if g.AutoRollback {
			// Reset cumulative excursion post-rollback while preserving baseline moments
			g.cumSum = 0
			g.minCumSum = 0
		}
	} else {
		g.driftTriggered = false
	}

	return driftDetected, statistic, lam
}

// Reset clears the sequential Page–Hinkley excursion state.
func (g *PageHinkleyDriftGuard) Reset() {
	g.cumSum = 0
	g.minCumSum = 0
	g.lastStat = 0
	g.driftTriggered = false
}

// Status returns a snapshot of the drift guard diagnostics.
func (g *PageHinkleyDriftGuard) Status() DriftGuardStatus {
	lam := g.Lambda
	if lam <= 0 {
		lam = 12.0
	}
	return DriftGuardStatus{
		DriftDetected:   g.driftTriggered,
		Statistic:       g.lastStat,
		Threshold:       lam,
		MeanResidualBps: g.meanRes,
		MeanFeatureNorm: g.meanFeat,
		RollbackCount:   g.rollbackCount,
		Samples:         g.count,
	}
}
