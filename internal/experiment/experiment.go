// Package experiment manages quantitative research experiments, model evaluation,
// parameter searches, and reproducibility guarantees for PiQuant.
package experiment

import (
	"errors"
	"fmt"
	"math"
	"time"

	"piquant-live/internal/models/common"
)

// State is the lifecycle state of an experiment or parameter search.
type State string

const (
	StateQueued      State = "queued"
	StateRunning     State = "running"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCancelled   State = "cancelled"
	StateInterrupted State = "interrupted"
)

// Metrics holds out-of-sample statistical evaluation and backtest simulation results.
// Evaluator measures statistical forecasting quality and risk-adjusted simulation metrics.
type Metrics struct {
	Samples             int                `json:"samples"`
	MAE                 float64            `json:"mae"`                  // Mean Absolute Error (bps)
	RMSE                float64            `json:"rmse"`                 // Root Mean Squared Error (bps)
	DirectionalAccuracy float64            `json:"directional_accuracy"` // Hit rate [0, 1]
	RankCorrelation     float64            `json:"rank_correlation"`     // Spearman's rho [-1, 1]
	InformationCoeff    float64            `json:"information_coeff"`    // Pearson correlation IC [-1, 1]
	SharpeRatio         float64            `json:"sharpe_ratio"`         // Statistical risk-adjusted return
	SortinoRatio        float64            `json:"sortino_ratio"`        // Downside risk-adjusted return
	MaxDrawdownBps      float64            `json:"max_drawdown_bps"`     // Peak-to-trough drawdown (bps)
	WinRate             float64            `json:"win_rate"`             // Win rate on directional bets [0, 1]
	ProfitFactor        float64            `json:"profit_factor"`        // Gross gains / gross losses
	EquityCurve         []float64          `json:"equity_curve"`         // Downsampled cumulative PnL (bps)
	BrierScore          float64            `json:"brier_score"`          // Probability calibration [0, 1]
	MeanForecast        float64            `json:"mean_forecast"`        // Mean predicted move (bps)
	MeanRealized        float64            `json:"mean_realized"`        // Mean actual move (bps)
	Baselines           map[string]float64 `json:"baselines"`            // Baseline MAE: zero, last-return, mean

	// Market Crash & Stablecoin Drop Sensitivity
	DownsideDeviation     float64 `json:"downside_deviation"`    // Semi-variance on negative moves (bps)
	CrashDrawdownVelocity float64 `json:"crash_velocity"`        // Maximum liquidation drawdown rate (bps)
	StablecoinDepegRisk   float64 `json:"stablecoin_depeg_risk"` // Basis dislocation risk under stablecoin discount (bps)
	RegimeCrashPct        float64 `json:"regime_crash_pct"`      // Proportion of samples in sharp downward shock

	// Institutional Rigor & Queue-Reactive Execution Metrics
	DeflatedSharpe      float64 `json:"deflated_sharpe"`       // Bailey & López de Prado Deflated Sharpe Ratio [0, 1]
	ProbBacktestOverfit float64 `json:"prob_backtest_overfit"` // Probability of Backtest Overfitting (PBO) [0, 1]
	PurgedCVMAE         float64 `json:"purged_cv_mae"`         // 5-Fold Purged & Embargoed Cross-Validation MAE (bps)
	MakerFillRate       float64 `json:"maker_fill_rate"`       // Queue-reactive passive limit order fill rate [0, 1]
	AdverseSelectionBps float64 `json:"adverse_selection_bps"` // Post-fill adverse selection drift against maker (bps)

	// Network Latency-Jitter Alpha Decay & Rolling Walk-Forward Validation
	AlphaDecay5msBps     float64 `json:"alpha_decay_5ms_bps"`    // Retained net trade edge under 5ms round-trip jitter (bps)
	AlphaDecay25msBps    float64 `json:"alpha_decay_25ms_bps"`   // Retained net trade edge under 25ms round-trip jitter (bps)
	AlphaDecay100msBps   float64 `json:"alpha_decay_100ms_bps"`  // Retained net trade edge under 100ms round-trip jitter (bps)
	SignalHalfLifeMs     float64 `json:"signal_half_life_ms"`    // Estimated alpha decay half-life (ms)
	WalkForwardIC        float64 `json:"walk_forward_ic"`        // Mean expanding/rolling walk-forward out-of-sample IC
	WalkForwardStability float64 `json:"walk_forward_stability"` // Walk-forward IC stability ratio (ICIR)

	// Post-Trade Markout Attribution & Split Conformal Uncertainty Gating
	SpreadCaptureBps    float64 `json:"spread_capture_bps"`    // Passive/quasi-passive half-spread capture edge (bps)
	DirectionalAlphaBps float64 `json:"directional_alpha_bps"` // Medium-horizon fundamental drift contribution (bps)
	MarkoutToxicityBps  float64 `json:"markout_toxicity_bps"`  // Immediate 1-tick adverse selection toxicity loss (bps)
	ConformalPassRate   float64 `json:"conformal_pass_rate"`   // Fraction of signals passing 90% split conformal gate [0, 1]
}

// SplitConfig defines chronological, non-overlapping dataset partitions.
// An embargo gap is enforced between partitions to prevent label lookahead leakage.
type SplitConfig struct {
	TrainPct      float64       `json:"train_pct"`      // e.g. 0.60 (60%)
	ValPct        float64       `json:"val_pct"`        // e.g. 0.20 (20%)
	TestPct       float64       `json:"test_pct"`       // e.g. 0.20 (20%)
	EmbargoWindow time.Duration `json:"embargo_window"` // Gap between splits >= forecast horizon
}

// DefaultSplit returns the standard safe 60/20/20 train/val/test split with 1m embargo.
func DefaultSplit(h common.Horizon) SplitConfig {
	embargo := h.Duration()
	if embargo < time.Minute {
		embargo = time.Minute
	}
	return SplitConfig{
		TrainPct:      0.60,
		ValPct:        0.20,
		TestPct:       0.20,
		EmbargoWindow: embargo,
	}
}

// Validate checks split proportions.
func (s SplitConfig) Validate() error {
	sum := s.TrainPct + s.ValPct + s.TestPct
	if math.Abs(sum-1.0) > 1e-4 {
		return fmt.Errorf("split percentages must sum to 1.0 (got %.3f)", sum)
	}
	if s.TrainPct < 0.20 {
		return errors.New("training split must be at least 20%")
	}
	if s.ValPct < 0.10 {
		return errors.New("validation split must be at least 10%")
	}
	if s.TestPct < 0.10 {
		return errors.New("held-out test split must be at least 10%")
	}
	return nil
}

// Experiment defines a single historical model evaluation job.
type Experiment struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Model       string         `json:"model"`       // e.g. "mlofi_ridge", "mlofi_logistic"
	FeatureSet  string         `json:"feature_set"` // e.g. "L1", "L5", "L20", "multi"
	Dataset     string         `json:"dataset"`     // path or name of dataset
	Horizon     common.Horizon `json:"horizon"`     // H30s, H1m, H5m
	Symbol      string         `json:"symbol"`      // e.g. "BTCUSDT"
	Params      map[string]any `json:"params"`      // hyperparameters (lambda, etc.)
	Split       SplitConfig    `json:"split"`
	State       State          `json:"state"`
	Progress    float64        `json:"progress"` // 0.0 to 1.0
	Stage       string         `json:"stage"`    // "replay", "feature_gen", "train", "val", "test"
	CreatedAt   time.Time      `json:"created_at"`
	StartedAt   time.Time      `json:"started_at,omitempty"`
	FinishedAt  time.Time      `json:"finished_at,omitempty"`
	DurationMs  int64          `json:"duration_ms"`
	WorkerNode  string         `json:"worker_node"` // "pi5" or "pi3b"

	// Results: Train, Validation, and Test metrics are strictly segregated.
	TrainMetrics Metrics `json:"train_metrics"`
	ValMetrics   Metrics `json:"val_metrics"`
	TestMetrics  Metrics `json:"test_metrics"`

	// GeneralizationDiagnostic warns when validation or test metrics diverge significantly from train.
	OverfitWarning string `json:"overfit_warning,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ComputeMetrics evaluates predicted vs actual basis points changes.
func ComputeMetrics(predicted, actual []float64) Metrics {
	n := len(predicted)
	if n == 0 || len(actual) != n {
		return Metrics{}
	}

	var sumAE, sumSE float64
	var sumPred, sumActual float64
	var correctDirection int
	var brierSum float64

	// Baseline sums
	var baselineZeroAE, baselineZeroSE float64
	var baselineMeanAE, baselineMeanSE float64
	for _, a := range actual {
		sumActual += a
	}
	meanActual := sumActual / float64(n)

	for i := 0; i < n; i++ {
		p := predicted[i]
		a := actual[i]

		err := p - a
		ae := math.Abs(err)
		se := err * err

		sumAE += ae
		sumSE += se
		sumPred += p

		// Directional hit rate
		if (p >= 0 && a >= 0) || (p < 0 && a < 0) {
			correctDirection++
		}

		// Probability calibration proxy: map bps to [0, 1] probability of upward move
		probUp := 1.0 / (1.0 + math.Exp(-p*0.5))
		actualUp := 0.0
		if a > 0 {
			actualUp = 1.0
		}
		brierSum += (probUp - actualUp) * (probUp - actualUp)

		// Baseline: zero
		baselineZeroAE += math.Abs(a)
		baselineZeroSE += a * a

		// Baseline: mean
		baselineMeanAE += math.Abs(meanActual - a)
		baselineMeanSE += (meanActual - a) * (meanActual - a)
	}

	mae := sumAE / float64(n)
	rmse := math.Sqrt(sumSE / float64(n))
	dirAcc := float64(correctDirection) / float64(n)
	brier := brierSum / float64(n)
	rho := spearmanRankCorrelation(predicted, actual)
	meanPred := sumPred / float64(n)

	// Information Coefficient (Pearson correlation r)
	var sumDevPredSq, sumDevActSq, sumDevProd float64
	for i := 0; i < n; i++ {
		dp := predicted[i] - meanPred
		da := actual[i] - meanActual
		sumDevPredSq += dp * dp
		sumDevActSq += da * da
		sumDevProd += dp * da
	}
	ic := 0.0
	denomIC := math.Sqrt(sumDevPredSq * sumDevActSq)
	if denomIC > 1e-12 {
		ic = sumDevProd / denomIC
	}

	// Backtest paper PnL simulation & Risk metrics
	var cumPnL, maxPeak, maxDD float64
	var grossGains, grossLosses float64
	var wins, activeSignals int
	returns := make([]float64, n)

	for i := 0; i < n; i++ {
		p := predicted[i]
		a := actual[i]
		var signal float64
		if p > 0 {
			signal = 1.0
		} else if p < 0 {
			signal = -1.0
		}

		if signal != 0 {
			activeSignals++
			ret := signal * a
			returns[i] = ret
			cumPnL += ret
			if ret > 0 {
				wins++
				grossGains += ret
			} else if ret < 0 {
				grossLosses += math.Abs(ret)
			}

			if cumPnL > maxPeak {
				maxPeak = cumPnL
			}
			dd := maxPeak - cumPnL
			if dd > maxDD {
				maxDD = dd
			}
		}
	}

	winRate := 0.0
	if activeSignals > 0 {
		winRate = float64(wins) / float64(activeSignals)
	}

	profitFactor := 1.0
	if grossLosses > 1e-9 {
		profitFactor = grossGains / grossLosses
	} else if grossGains > 0 {
		profitFactor = 99.9
	}

	// Downsampled equity curve for rapid rendering
	curveStep := int(math.Ceil(float64(n) / 60.0))
	if curveStep < 1 {
		curveStep = 1
	}
	equityCurve := make([]float64, 0, 60)
	running := 0.0
	for i := 0; i < n; i++ {
		running += returns[i]
		if i%curveStep == 0 || i == n-1 {
			equityCurve = append(equityCurve, math.Round(running*100)/100)
		}
	}

	var sharpe, sortino, deflatedSharpe float64
	if activeSignals > 5 {
		meanRet := cumPnL / float64(activeSignals)
		var varRet, varDown float64
		for i := 0; i < n; i++ {
			if returns[i] != 0 {
				diff := returns[i] - meanRet
				varRet += diff * diff
				if returns[i] < 0 {
					varDown += returns[i] * returns[i]
				}
			}
		}
		stdRet := math.Sqrt(varRet / float64(activeSignals))
		stdDown := math.Sqrt(varDown / float64(activeSignals))
		if stdRet > 1e-6 {
			sharpe = (meanRet / stdRet) * math.Sqrt(math.Min(float64(activeSignals), 252))
			// Compute higher moments (skewness & kurtosis) for Deflated Sharpe Ratio
			var m3, m4 float64
			for i := 0; i < n; i++ {
				if returns[i] != 0 {
					z := (returns[i] - meanRet) / stdRet
					z2 := z * z
					m3 += z2 * z
					m4 += z2 * z2
				}
			}
			skewness := m3 / float64(activeSignals)
			kurtosis := m4 / float64(activeSignals)
			deflatedSharpe = ComputeDeflatedSharpe(sharpe, activeSignals, skewness, kurtosis, 10)
		}
		if stdDown > 1e-6 {
			sortino = (meanRet / stdDown) * math.Sqrt(math.Min(float64(activeSignals), 252))
		}
	}

	// Downside Risk, Market Drop & Stablecoin De-peg Sensitivity
	var sumDownSq float64
	var downCount int
	var crashShocks int
	var maxDropVelocity float64

	var sumActSq float64
	for _, a := range actual {
		diff := a - meanActual
		sumActSq += diff * diff
	}
	stdActual := math.Sqrt(sumActSq / float64(n))
	crashThreshold := -1.8 * stdActual
	if crashThreshold > -1.2 {
		crashThreshold = -1.2
	}

	for i := 0; i < n; i++ {
		a := actual[i]
		if a < 0 {
			sumDownSq += a * a
			downCount++
		}
		if a < crashThreshold {
			crashShocks++
		}
		if i >= 4 {
			windowDrop := (actual[i] + actual[i-1] + actual[i-2] + actual[i-3] + actual[i-4]) / 5.0
			if windowDrop < 0 && math.Abs(windowDrop) > maxDropVelocity {
				maxDropVelocity = math.Abs(windowDrop)
			}
		}
	}

	downsideDev := 0.0
	if downCount > 0 {
		downsideDev = math.Sqrt(sumDownSq / float64(downCount))
	}
	regimeCrashPct := float64(crashShocks) / float64(n)
	stableDepegRisk := math.Abs(meanActual)*0.2 + downsideDev*0.25 + (20.0 * regimeCrashPct)

	return Metrics{
		Samples:               n,
		MAE:                   mae,
		RMSE:                  rmse,
		DirectionalAccuracy:   dirAcc,
		RankCorrelation:       rho,
		InformationCoeff:      ic,
		SharpeRatio:           sharpe,
		SortinoRatio:          sortino,
		MaxDrawdownBps:        maxDD,
		WinRate:               winRate,
		ProfitFactor:          profitFactor,
		EquityCurve:           equityCurve,
		BrierScore:            brier,
		MeanForecast:          meanPred,
		MeanRealized:          meanActual,
		DownsideDeviation:     downsideDev,
		CrashDrawdownVelocity: maxDropVelocity,
		StablecoinDepegRisk:   stableDepegRisk,
		RegimeCrashPct:        regimeCrashPct,
		DeflatedSharpe:        deflatedSharpe,
		Baselines: map[string]float64{
			"zero_mae":  baselineZeroAE / float64(n),
			"zero_rmse": math.Sqrt(baselineZeroSE / float64(n)),
			"mean_mae":  baselineMeanAE / float64(n),
			"mean_rmse": math.Sqrt(baselineMeanSE / float64(n)),
		},
	}
}

// EvaluatePredictions computes full statistical metrics plus realistic network latency-jitter
// alpha decay profiles (5ms, 25ms, 100ms), signal half-life, and post-trade markout attribution
// for a slice of samples.
func EvaluatePredictions(samples []common.Sample, predicted []float64) Metrics {
	actual := make([]float64, len(samples))
	for i := range samples {
		actual[i] = samples[i].Y
	}
	m := ComputeMetrics(predicted, actual)
	d5, d25, d100, hl := SimulateLatencyAlphaDecay(samples, predicted, 0.05, 0.20)
	m.AlphaDecay5msBps = d5
	m.AlphaDecay25msBps = d25
	m.AlphaDecay100msBps = d100
	m.SignalHalfLifeMs = hl

	spCap, dirAlpha, toxLoss, confPass := ComputeMarkoutAttribution(samples, predicted, 0.05, 0.20)
	m.SpreadCaptureBps = spCap
	m.DirectionalAlphaBps = dirAlpha
	m.MarkoutToxicityBps = toxLoss
	m.ConformalPassRate = confPass
	return m
}

// spearmanRankCorrelation calculates the rank correlation between two vectors.
func spearmanRankCorrelation(x, y []float64) float64 {
	n := len(x)
	if n < 2 {
		return 0
	}
	items := make([]rankItem, n)
	rx := make([]float64, n)
	ry := make([]float64, n)
	rankInto(x, rx, items)
	rankInto(y, ry, items)

	var d2 float64
	for i := 0; i < n; i++ {
		d := rx[i] - ry[i]
		d2 += d * d
	}
	return 1.0 - (6.0*d2)/(float64(n)*(float64(n*n)-1.0))
}

type rankItem struct {
	val float64
	idx int
}

func rankInto(v []float64, ranks []float64, items []rankItem) {
	n := len(v)
	for i := 0; i < n; i++ {
		items[i] = rankItem{val: v[i], idx: i}
	}
	sortRankItems(items)
	for i := 0; i < n; {
		j := i + 1
		for j < n && items[j].val == items[i].val {
			j++
		}
		avgRank := 1.0 + float64(i+j-1)/2.0
		for k := i; k < j; k++ {
			ranks[items[k].idx] = avgRank
		}
		i = j
	}
}

func rank(v []float64) []float64 {
	n := len(v)
	ranks := make([]float64, n)
	items := make([]rankItem, n)
	rankInto(v, ranks, items)
	return ranks
}

// sortRankItems is an allocation-free quicksort with an insertion sort base case,
// avoiding reflection and interface boxing overhead.
func sortRankItems(a []rankItem) {
	if len(a) < 16 {
		for i := 1; i < len(a); i++ {
			tmp := a[i]
			j := i
			for j > 0 && a[j-1].val > tmp.val {
				a[j] = a[j-1]
				j--
			}
			a[j] = tmp
		}
		return
	}
	pivot := a[len(a)/2].val
	left, right := 0, len(a)-1
	for left <= right {
		for a[left].val < pivot {
			left++
		}
		for a[right].val > pivot {
			right--
		}
		if left <= right {
			a[left], a[right] = a[right], a[left]
			left++
			right--
		}
	}
	if right > 0 {
		sortRankItems(a[:right+1])
	}
	if left < len(a) {
		sortRankItems(a[left:])
	}
}

// CheckGeneralization compares train, validation, and test metrics and produces
// an actionable diagnostic if signs of severe overfitting or failure to generalize occur.
func CheckGeneralization(train, val, test Metrics) string {
	if train.Samples == 0 || val.Samples == 0 {
		return ""
	}
	// If validation MAE is more than 35% worse than training MAE
	if val.MAE > train.MAE*1.35 {
		return fmt.Sprintf("High validation degradation (val MAE %.2f bps vs train MAE %.2f bps). Model may be overfitting training noise.", val.MAE, train.MAE)
	}
	// If directional accuracy drops below random on validation
	if train.DirectionalAccuracy > 0.55 && val.DirectionalAccuracy < 0.49 {
		return fmt.Sprintf("Directional accuracy collapse: train %.1f%% vs val %.1f%%. Signal fails to generalize out of sample.", train.DirectionalAccuracy*100, val.DirectionalAccuracy*100)
	}
	// If rank correlation is negative on validation
	if train.RankCorrelation > 0.10 && val.RankCorrelation < -0.05 {
		return fmt.Sprintf("Negative rank correlation on validation (rho = %.2f). Inverting signal out of sample.", val.RankCorrelation)
	}
	return ""
}
