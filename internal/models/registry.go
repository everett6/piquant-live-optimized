// Package models provides the central registry and metadata for all quantitative models
// evaluated by the PiQuant research platform.
package models

import (
	"piquant-live/internal/features"
	"piquant-live/internal/models/common"
	"piquant-live/internal/models/mlofi"
)

// HorizonCategory classifies model timeframes for the research platform.
type HorizonCategory string

const (
	HorizonMinutes HorizonCategory = "Minutes"
	HorizonHours   HorizonCategory = "Hours"
	HorizonDays    HorizonCategory = "Days"
	HorizonWeeks   HorizonCategory = "Weeks"
)

// ModelSpec defines a model's specifications, mathematical assumptions, and limitations.
type ModelSpec struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Category    HorizonCategory `json:"category"`
	Horizons    []string        `json:"horizons"`
	Features    []string        `json:"features"`
	Algorithm   string          `json:"algorithm"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Assumptions string          `json:"assumptions"`
	Limitations string          `json:"limitations"`
	Calibration string          `json:"calibration"`
}

// Registry returns the catalog of all quantitative models in PiQuant.
func Registry() []ModelSpec {
	return []ModelSpec{
		// 1. Minutes Horizon: Multi-level OFI Ridge
		{
			ID:          "mlofi_ridge",
			Name:        "Multi-Level OFI Ridge",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "imb_l5", "imb_l20", "mlofi_1s", "mlofi_5s"},
			Algorithm:   "L2 Regularized Ridge Regression (normal equations via Cholesky)",
			Version:     "v1.2.0",
			Description: "Predicts high-frequency mid-price return using exponentially decaying multi-level order flow imbalance and microprice deviation.",
			Assumptions: "Linear price impact of net queue volume changes; stationary volatility over sub-minute windows.",
			Limitations: "Sensitive to sudden spread widening; does not account for latent iceberg orders or non-displayed dark liquidity.",
			Calibration: "Calibrated via out-of-sample Brier score and Platt probability scaling.",
		},
		// 2. Minutes Horizon: Elastic Net Sparse Feature Selector
		{
			ID:          "elastic_net",
			Name:        "Elastic Net Coordinate Descent",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "imb_l5", "imb_l20", "mlofi_1s", "mlofi_5s"},
			Algorithm:   "L1/L2 Elastic Net via Fast Cyclical Coordinate Descent (Soft-Thresholding)",
			Version:     "v1.1.0",
			Description: "Balances sparsity (L1 Lasso) and collinearity grouping (L2 Ridge) to isolate robust order book signals.",
			Assumptions: "True alpha is sparse; non-informative order book noise features should have coefficients shrunk to zero.",
			Limitations: "Sensitive to choice of mixing ratio alpha.",
			Calibration: "Coordinate descent converges to global optimum with duality gap < 1e-6.",
		},
		// 3. Minutes Horizon: Adaptive Online RLS
		{
			ID:          "adaptive_rls",
			Name:        "Adaptive Recursive Least Squares",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s"},
			Algorithm:   "Exponentially Weighted RLS with Forgetting Factor (Kalman filter formulation)",
			Version:     "v1.0.0",
			Description: "Online sequential parameter updates that track non-stationary microstructure regime transitions in real time.",
			Assumptions: "Recent order flow observations are more predictive of immediate price drift than distant historical prints.",
			Limitations: "Can over-react to transitory noise if forgetting factor is too small (< 0.95).",
			Calibration: "Dynamically adjusted covariance matrix with numerical symmetrization.",
		},
		// 4. Minutes Horizon: Queue Imbalance Logistic Classifier
		{
			ID:          "queue_logistic",
			Name:        "Queue-Reactive Logistic Direction",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m"},
			Features:    []string{"queue_ratio", "imb_l1", "micro_dev", "spread_bps"},
			Algorithm:   "L2 Regularized Binary Logistic Regression (IRLS)",
			Version:     "v1.0.4",
			Description: "Estimates the probability that the next tick or mid move is upward, conditioned on queue depletion at the touch.",
			Assumptions: "Markovian queue transitions; order cancellations are independent of recent fill rate.",
			Limitations: "Under-predicts adverse selection during high-volume news announcements.",
			Calibration: "Reliability diagram shows < 2% calibration error within [0.35, 0.65] predicted probability.",
		},
		// 5. Minutes Horizon: Meta-Labeling Fee-Gated Overlay
		{
			ID:          "meta_labeler",
			Name:        "Meta-Labeling Fee-Gated Filter",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "trade_cnt_1s"},
			Algorithm:   "Two-Stage López de Prado Meta-Labeling with Half-Kelly Bet Sizing",
			Version:     "v1.0.0",
			Description: "Applies a secondary calibrated probability filter to eliminate low-conviction signals that lose to taker fees.",
			Assumptions: "Primary model produces directional drift; secondary classifier predicts probability of exceeding round-trip execution drag.",
			Limitations: "Reduces trade frequency by 60-80% to preserve capital for high-conviction market regimes.",
			Calibration: "Thresholded at P(gain > fees) >= 0.55 with fractional Kelly conviction sizing.",
		},
		// 6. Minutes Horizon: GLFT Queue-Reactive Passive Market Maker
		{
			ID:          "glft_maker",
			Name:        "GLFT Queue-Reactive Market Maker",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "mlofi_1s", "vol_30s_bps", "trade_imb_5s", "queue_ratio"},
			Algorithm:   "Guéant-Lehalle-Fernandez-Tapia (GLFT) / Avellaneda-Stoikov Inventory-Skewed Reservation Price",
			Version:     "v1.1.0",
			Description: "Computes optimal passive bid/ask reservation prices and half-spreads with MLOFI alpha skew, Stoikov microprice, and queue-reactive fill simulation.",
			Assumptions: "Exponential fill intensity decay with distance from mid; adverse selection proportional to trade flow toxicity.",
			Limitations: "Queue priority estimation assumes FIFO matching without hidden dark-pool priority jumps.",
			Calibration: "Empirically calibrated book intensity kappa and post-fill adverse selection drift.",
		},
		// 7. Minutes Horizon: Online Regime-Switching Dynamic Ensemble
		{
			ID:          "regime_ensemble",
			Name:        "BOCPD Regime-Switching Ensemble",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "imb_l5", "micro_dev", "spread_bps", "mlofi_1s", "mlofi_5s", "vol_30s_bps", "ret_1s_bps", "trade_imb_5s"},
			Algorithm:   "Bayesian Online Changepoint Detection (BOCPD) + Exponentially Weighted Model Averaging (Ridge + ElasticNet + GLFT)",
			Version:     "v1.1.0",
			Description: "Dynamically classifies market state into Calm Maker, Directional Flow, or Toxic Cascade and blends three specialized experts.",
			Assumptions: "Microstructure alternates between stationary liquidity provision regimes and self-exciting directional sweeps.",
			Limitations: "Requires higher compute per sample to fit three constituent sub-experts simultaneously.",
			Calibration: "Online exponential loss weighting w_{m,t+1} = w_{m,t} exp(-eta * L_{m,t}).",
		},
		// 8. Minutes Horizon: Perpetual Basis & Liquidation Cascade Reversion
		{
			ID:          "perp_liquidation_reversion",
			Name:        "Perp Basis & Liquidation Reversion",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_30s", "mlofi_1s", "vol_30s_bps", "ret_1s_bps", "trade_imb_5s", "trade_cnt_1s"},
			Algorithm:   "Regularized Ridge + Non-Linear Liquidation Cascade Exhaustion Reversion & Perpetual Basis Carry",
			Version:     "v1.2.0",
			Description: "Detects forced margin liquidation cascades and captures post-cascade V-shaped exhaustion reversion combined with perpetual funding basis carry.",
			Assumptions: "Forced liquidations create temporary price dislocations beyond fundamental liquidity equilibrium that revert upon exhaustion.",
			Limitations: "During prolonged macro insolvency events, cascades may chain without immediate V-shaped recovery.",
			Calibration: "Least-squares residual calibration of liquidation exhaustion alpha and EWMA basis z-score pull.",
		},
		// 9. Minutes Horizon: Truncated Depth-2 Path Signatures (Lévy Area) & Kyle's Lambda
		{
			ID:          "rough_signature_ofi",
			Name:        "Rough Path Signature OFI & Kyle Lambda",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "imb_l5", "imb_l20", "micro_dev", "ofi_1s", "mlofi_1s", "ret_1s_bps", "trade_imb_5s"},
			Algorithm:   "Truncated Depth-2 Path Signature (Lévy Area Cross-Terms) + Dynamic Kyle's Lambda Price Impact Damping",
			Version:     "v1.3.0",
			Description: "Captures non-commutative lead-lag order flow trajectories via Depth-2 Lévy area path signatures and dampens predictions under high Kyle price impact.",
			Assumptions: "Order book lead-lag rotational curl between touch OFI, deep depth, and Hawkes trade intensity carries predictive power beyond linear state snapshots.",
			Limitations: "Depth-2 truncation omits third-order rough path iterated integrals during multi-minute regime shifts.",
			Calibration: "Orthogonalized L2 residual projection on standardized Depth-2 Lévy area features with Kyle impact elasticity.",
		},
		// 10. Minutes Horizon: Cartea-Jaimungal HJB Target Inventory & Adverse Selection
		{
			ID:          "cartea_jaimungal",
			Name:        "Cartea-Jaimungal HJB Market Maker",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"spread_bps", "ofi_1s", "vol_30s_bps", "imb_l1", "micro_dev"},
			Algorithm:   "Cartea-Jaimungal Closed-Form HJB Target Inventory Penalty & Adverse Selection OFI Skew",
			Version:     "v1.0.0",
			Description: "Calculates optimal reservation price and asymptotic half-spreads accounting for running inventory penalty, terminal inventory target deviation, and market impact.",
			Assumptions: "Quadratic running and terminal inventory penalties; linear adverse selection market impact from OFI.",
			Limitations: "Assumes continuous Poisson fill intensities without discrete queue priority jumps.",
			Calibration: "Calibrated via running penalty phi, terminal penalty alpha, and liquidity intensity kappa.",
		},
		// 11. Minutes Horizon: Echo State Network (ESN) / Reservoir Computing Microstructure Model
		{
			ID:          "esn_reservoir",
			Name:        "Echo State Reservoir Microstructure",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "imb_l5", "imb_l20", "micro_dev", "spread_bps", "ofi_1s", "mlofi_1s", "vol_30s_bps", "ret_1s_bps", "trade_imb_5s"},
			Algorithm:   "Leaky Echo State Network (N_res=16, rho=0.85, alpha=0.35) + Direct Skip Ridge Readout",
			Version:     "v1.0.0",
			Description: "Projects 16 core microstructure features into a deterministic cyclic-chord recurrent reservoir with leaky tanh dynamics and direct linear skip connections.",
			Assumptions: "Short-term order book memory satisfies the Echo State Property (spectral radius < 1) and separates non-linear flow trajectories in reservoir state space.",
			Limitations: "Fixed reservoir topology does not adapt recurrent weights online; relies on regularized readout calibration.",
			Calibration: "Solved in closed form via L2-regularized Gauss-Jordan normal equations over standardized reservoir states and skip residuals.",
		},
		// 12. Minutes Horizon: Spot vs. Perpetual Futures Multi-Stream Basis Arbitrage
		{
			ID:          "spot_perp_basis",
			Name:        "Spot-Perp Basis Arbitrage & Mean Reversion",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "ret_1s_bps", "trade_imb_5s"},
			Algorithm:   "Spot-Perp Basis Tracker + Welford EWMA Z-Score + Mean-Reverting Basis Alpha",
			Version:     "v1.0.0",
			Description: "Tracks spot vs. perpetual futures price divergence, standardized basis Z-score, and executes statistical basis convergence arbitrage with maker inventory skew.",
			Assumptions: "Basis dislocations revert to cost-of-carry equilibrium over intraday horizons; cointegrated spot and perpetual liquidity.",
			Limitations: "Under extreme funding rate regimes or venue disconnections, basis divergence may widen before converging.",
			Calibration: "Welford EWMA variance with adaptive mean-reversion speed theta.",
		},
		// 13. Minutes Horizon: Triangular Cross-Rate Synthetic Arbitrage Model
		{
			ID:          "triangular_arb",
			Name:        "Triangular Cross-Rate Synthetic Arbitrage",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "ret_1s_bps"},
			Algorithm:   "Triangular Cross-Rate Synthetic Arbitrage + Multi-Leg Fee Hurdle",
			Version:     "v1.0.0",
			Description: "Identifies synthetic vs. direct cross-rate pricing dislocations across a 3-asset triangular loop (BTCUSDT, ETHUSDT, ETHBTC) and evaluates net arbitrage edge net of fees.",
			Assumptions: "Cross-rate pricing disparity closes via liquidity flow across triangular legs; fee hurdles prevent unprofitable execution.",
			Limitations: "Simultaneous execution risk (leg risk) across 3 distinct order books during high volatility.",
			Calibration: "Maker/taker fee schedule and divergence threshold calibrated per venue fee tiers.",
		},
		// 14. Minutes Horizon: Cross-Venue Lead-Lag Arbitrage Engine
		{
			ID:          "cross_venue_leadlag",
			Name:        "Cross-Venue Lead-Lag Arbitrage Engine",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "ret_1s_bps", "ret_5s_bps"},
			Algorithm:   "High-Frequency Hasbrouck Information Share + Microsecond Tick Lead-Lag Drift",
			Version:     "v1.0.0",
			Description: "Monitors cross-venue price discovery (e.g. Binance vs. Bybit), computes Hasbrouck Information Share and tick-lead correlation, forecasting immediate follower price drift.",
			Assumptions: "Permanent price innovations originate first on the leader venue; follower venue order book reflects innovations with microsecond to millisecond latency.",
			Limitations: "Subject to cross-venue latency spikes, WebSocket reconnection jitter, and exchange downtime.",
			Calibration: "EWMA innovation covariance with adaptive Hasbrouck information share bounds.",
		},
		// 15. Minutes Horizon: Online VPIN Toxicity Model
		{
			ID:          "vpin_toxicity",
			Name:        "Volume-Synchronized Probability of Toxicity (VPIN)",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "ret_1s_bps", "trade_imb_5s", "trade_cnt_1s"},
			Algorithm:   "Easley-López de Prado-O'Hara (2012) Volume-Clock Toxicity Estimator + Adverse Selection Damping",
			Version:     "v1.0.0",
			Description: "Measures informed flow toxicity via rolling volume-clock order imbalance buckets and widens quotes or dampens alpha to prevent adverse selection.",
			Assumptions: "Informed traders trade with high urgency and directional clustering, skewing order imbalance within fixed volume buckets.",
			Limitations: "Bucket volume V must be calibrated to venue turnover; sensitive to high-frequency wash trading or iceberg fragmentation.",
			Calibration: "Calibrated volume bucket V=5.0 BTC over N=50 rolling buckets with 0.70 toxicity threshold.",
		},
		// 16. Minutes Horizon: Real-time Kyle's Lambda & Market Impact Sizer
		{
			ID:          "kyles_lambda",
			Name:        "Kyle's Lambda & Market Impact Sizer",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "ret_1s_bps", "trade_imb_5s"},
			Algorithm:   "Kyle (1985) Microstructural Price Impact Coefficient via Recursive Online EWMA Covariance",
			Version:     "v1.0.0",
			Description: "Estimates marginal price impact per unit volume in real time and sizes maker/taker orders under an adverse selection impact budget.",
			Assumptions: "Linear price response to net signed order flow; noise traders provide inelastic counterparty liquidity.",
			Limitations: "Assumes symmetric impact between buy and sell flow; square-root law dominates for very large block trades.",
			Calibration: "Recursive EWMA covariance and variance estimation with forgetting factor alpha=0.05 and epsilon=1e-6.",
		},
		// 17. Minutes Horizon: Regime-Switching Adaptive Meta-Ensemble
		{
			ID:          "adaptive_meta_ensemble",
			Name:        "Regime-Switching Adaptive Meta-Ensemble",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "mlofi_1s", "vol_30s_bps", "ret_1s_bps", "ret_5s_bps", "trade_imb_5s", "trade_cnt_1s", "queue_ratio"},
			Algorithm:   "Multiplicative Weights / Discounted Hedge Online Meta-Learner + 3-Regime Microstructure Classifier",
			Version:     "v1.0.0",
			Description: "Dynamically classifies order book state into LowVol, Trending, or Cascade regimes and blends 6 alpha families via online exponential weight updating.",
			Assumptions: "Alpha efficacy varies systematically across volatility and jump regimes; online discounted hedge bounds cumulative meta-regret.",
			Limitations: "Sudden regime switching can temporarily misallocate expert weights during transition latency.",
			Calibration: "Exponential decay gamma=0.98, online learning rate eta=0.15, calibrated on 30s/1m/5m returns.",
		},
		// 18. Minutes Horizon: Perpetual Funding Rate & Carry Harvesting Engine
		{
			ID:          "funding_carry",
			Name:        "Perpetual Funding Rate & Carry Harvester",
			Category:    HorizonMinutes,
			Horizons:    []string{"30s", "1m", "5m"},
			Features:    []string{"imb_l1", "micro_dev", "spread_bps", "ofi_1s", "ofi_5s", "vol_30s_bps", "ret_1s_bps", "ret_5s_bps"},
			Algorithm:   "Mark-Index Premium Tracker + Clamped 8h EWMA Funding + Carry Reservation Skew",
			Version:     "v1.0.0",
			Description: "Tracks spot-perpetual premium divergence, predicts 8h funding rates, and skews maker reservation prices to harvest rebates while penalizing paying inventory.",
			Assumptions: "Funding payments drive systematic basis drift toward settlement; maker inventory on rebate side yields positive carry.",
			Limitations: "Extreme basis dislocation during liquidation cascades can exceed funding rebate yields.",
			Calibration: "Exchange damping clamp band +/-0.05%, baseline 0.01% interest, EWMA alpha=0.05.",
		},
		// 8. Hours Horizon: Volatility Regime Filter
		{
			ID:          "vol_regime_ridge",
			Name:        "EWMA Volatility Flow Estimator",
			Category:    HorizonHours,
			Horizons:    []string{"1h", "4h"},
			Features:    []string{"volatility", "spread_bps", "trade_cnt_1s", "trade_imb_5s"},
			Algorithm:   "Adaptive Ridge Regression with Welford-normalized rolling EWMA volatility",
			Version:     "v0.9.1",
			Description: "Forecasts medium-frequency price momentum conditioned on realized variance regimes.",
			Assumptions: "Volatility clustering (ARCH-type effects); volume-synchronized probability of toxicity.",
			Limitations: "Regime transitions can lag abrupt trend reversals by up to one rolling window.",
			Calibration: "Quantile-binned error distribution with student-t tails.",
		},
		// 4. Days Horizon: Cross-Impact Reversion
		{
			ID:          "reversion_daily",
			Name:        "Microstructure Residual Reversion",
			Category:    HorizonDays,
			Horizons:    []string{"1d"},
			Features:    []string{"micro_dev", "spread_bps", "volatility", "imb_l20"},
			Algorithm:   "Multi-factor Residual Mean Reversion",
			Version:     "v0.8.0",
			Description: "Exploits multi-day inventory imbalances and institutional execution drift.",
			Assumptions: "Large participant inventory rebalancing occurs over multiple trading sessions.",
			Limitations: "Structural regime shifts (e.g. forks, macro policy shocks) produce persistent drift.",
			Calibration: "Purged K-fold cross-validation with 2-hour embargo buffers.",
		},
		// 5. Weeks Horizon: Multi-Factor Volume Regime
		{
			ID:          "volume_regime_weekly",
			Name:        "Regime-Aware Multi-Factor Model",
			Category:    HorizonWeeks,
			Horizons:    []string{"1w"},
			Features:    []string{"volatility", "trade_imb_5s", "spread_bps"},
			Algorithm:   "Hierarchical Ridge Model with Regime Conditioning",
			Version:     "v0.7.2",
			Description: "Longer-horizon research baseline tracking low-frequency order flow aggregates across weeks.",
			Assumptions: "Long-term stationarity of normalized feature distributions.",
			Limitations: "Limited sample size in local capture files; designed as an educational reference.",
			Calibration: "Walk-forward validation over rolling quarterly slices.",
		},
	}
}

// ModelByID returns the spec for a specific model ID.
func ModelByID(id string) (ModelSpec, bool) {
	for _, m := range Registry() {
		if m.ID == id {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// AvailableFeatureSets returns the feature sets for MLOFI.
func AvailableFeatureSets() []string {
	return []string{
		string(mlofi.SetL1),
		string(mlofi.SetL5),
		string(mlofi.SetL20),
		string(mlofi.SetMulti),
	}
}

// FeatureNames returns the 16 core feature names.
func FeatureNames() []string {
	return features.Names
}

// HorizonsByCategory returns model horizons.
func HorizonsByCategory(cat HorizonCategory) []common.Horizon {
	switch cat {
	case HorizonMinutes:
		return []common.Horizon{common.H30s, common.H1m, common.H5m}
	default:
		return []common.Horizon{common.H5m}
	}
}
