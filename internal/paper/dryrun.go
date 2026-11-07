package paper

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/experiment"
	"piquant-live/internal/models/common"
)

// DryRunConfig configures the 14-day autonomous paper money dry run.
type DryRunConfig struct {
	InitialCapital  float64       // Initial paper money in USDT (default: 100,000.00)
	TargetDuration  time.Duration // Target autonomous duration (default: 14 * 24 * time.Hour = 336h)
	Symbols         []string      // Instruments traded (default: BTCUSDT, ETHUSDT)
	MaxDrawdownPct  float64       // Max allowable drawdown % before circuit breaker halts (default: 2.0%)
	MakerFeeBps     float64       // Maker fee/rebate in bps (default: 0.0)
	TakerFeeBps     float64       // Taker fee in bps (default: 4.0)
	KylesLambda     float64       // Slippage coefficient on taker orders (bps per unit volume, default: 0.25)
	StateFile       string        // Path to checkpoint state (default: data/paper/dryrun_state.json)
	JournalFile     string        // Path to trade journal (default: data/paper/dryrun_journal.jsonl)
	CheckpointFreq  time.Duration // Interval to save state (default: 5 * time.Second)
	OrderLatency     time.Duration // Feed + order placement latency before maker order is active (default: 50ms)
	EnableAllModels   bool          // Re-enable models disabled by ablation gate in the ensemble (default: false)
	ExtendOnShutdown  bool          // Automatically extend target end time by offline duration upon shutdown/power-cycle recovery (default: true)
	FlattenOnShutdown bool          // Automatically liquidate all open positions to cash at current mark price on shutdown (default: true)
	WindowPreset      string        // Window preset: "7d" (last week), "30d" (last month), "14d" (2 weeks)
}

// DefaultDryRunConfig returns standard institutional configuration for the 2-week run.
func DefaultDryRunConfig(dataDir string) DryRunConfig {
	if dataDir == "" {
		dataDir = "data"
	}
	paperDir := filepath.Join(dataDir, "paper")
	return DryRunConfig{
		InitialCapital:    1000.00,
		TargetDuration:    14 * 24 * time.Hour,
		Symbols:           []string{"BTCUSDT", "ETHUSDT"},
		MaxDrawdownPct:    2.0,
		MakerFeeBps:       0.0,
		TakerFeeBps:       4.0,
		KylesLambda:       0.25,
		StateFile:         filepath.Join(paperDir, "dryrun_state.json"),
		JournalFile:       filepath.Join(paperDir, "dryrun_journal.jsonl"),
		CheckpointFreq:    5 * time.Second,
		OrderLatency:      50 * time.Millisecond,
		ExtendOnShutdown:  true,
		FlattenOnShutdown: true,
	}
}

// Position tracks an individual symbol's inventory and execution metrics.
type Position struct {
	Symbol          string  `json:"symbol"`
	Size            float64 `json:"size"`              // Quantity (positive: long, negative: short)
	AvgEntryPrice   float64 `json:"avg_entry_price"`   // Volume-weighted entry price
	MarkPrice       float64 `json:"mark_price"`        // Latest mid price
	UnrealizedPnL   float64 `json:"unrealized_pnl"`    // Mark-to-market PnL
	RealizedPnL     float64 `json:"realized_pnl"`      // Cumulative realized PnL
	TotalTrades     int64   `json:"total_trades"`      // Total completed fills
	MakerTrades     int64   `json:"maker_trades"`      // Maker fills
	TakerTrades     int64   `json:"taker_trades"`      // Taker fills
	TotalVolumeUSDT float64 `json:"total_volume_usdt"` // Cumulative traded notional
}

// TradeRecord represents an individual executed paper trade for the journal.
type TradeRecord struct {
	Timestamp   time.Time `json:"timestamp"`
	Symbol      string    `json:"symbol"`
	Side        string    `json:"side"` // "BUY" or "SELL"
	Role        string    `json:"role"` // "MAKER" or "TAKER"
	Price       float64   `json:"price"`
	Size        float64   `json:"size"`
	ValueUSDT   float64   `json:"value_usdt"`
	FeeUSDT     float64   `json:"fee_usdt"`
	EdgeBps     float64   `json:"edge_bps"`
	SlippageBps float64   `json:"slippage_bps"`
	PnLRealized float64   `json:"pnl_realized"`
	Strategy    string    `json:"strategy"`
	DataSource  string    `json:"data_source"` // "live" or "fallback"
}

// ActiveMakerOrder tracks an active resting limit maker order in the paper book.
type ActiveMakerOrder struct {
	Symbol     string    `json:"symbol"`
	Side       string    `json:"side"` // "BUY" or "SELL"
	Price      float64   `json:"price"`
	Size       float64   `json:"size"`
	PlacedAt   time.Time `json:"placed_at"`
	ActiveAt   time.Time `json:"active_at"` // PlacedAt + OrderLatency
	QueueAhead float64   `json:"queue_ahead"`
	Strategy   string    `json:"strategy"`
	EdgeBps    float64   `json:"edge_bps"`
}

// DryRunState is the checkpointable state of the 14-day paper trading engine.
type DryRunState struct {
	StartTime         time.Time            `json:"start_time"`
	TargetEndTime     time.Time            `json:"target_end_time"`
	LastCheckpoint    time.Time            `json:"last_checkpoint"`
	InitialCapital    float64              `json:"initial_capital"`
	Cash              float64              `json:"cash"`
	Equity            float64              `json:"equity"`
	PeakEquity        float64              `json:"peak_equity"`
	RealizedPnL       float64              `json:"realized_pnl"`
	UnrealizedPnL     float64              `json:"unrealized_pnl"`
	TotalReturnPct    float64              `json:"total_return_pct"`
	MaxDrawdownPct    float64              `json:"max_drawdown_pct"`
	CircuitHalted     bool                 `json:"circuit_halted"`
	CircuitHaltReason string               `json:"circuit_halt_reason"`
	TotalTrades       int64                `json:"total_trades"`
	WinningTrades     int64                `json:"winning_trades"`
	LosingTrades      int64                `json:"losing_trades"`
	WinRate           float64              `json:"win_rate"`
	SharpeRatio       float64              `json:"sharpe_ratio"`
	Positions         map[string]*Position `json:"positions"`
	DailyReturns      []float64            `json:"daily_returns"`
	DaysElapsed       int                  `json:"days_elapsed"`
	TargetDays        int                  `json:"target_days"`              // Total days in target evaluation window (e.g. 7, 14, 30)
	WindowPreset      string               `json:"window_preset"`            // "7d" (last week), "30d" (last month), "14d" (2 weeks)
	HoursRemaining    float64              `json:"hours_remaining"`
	DataSource             string               `json:"data_source"`              // "live" or "fallback"
	FeedDownSeconds        float64              `json:"feed_down_seconds"`        // Cumulative duration market feed was disconnected
	QuotingPaused          bool                 `json:"quoting_paused"`           // True when quoting/matching paused due to feed down
	OfflineDowntimeSeconds float64              `json:"offline_downtime_seconds"` // Cumulative time system was shut down / offline
	ShutdownCount          int                  `json:"shutdown_count"`           // Number of power-cycles/resumptions survived
	LastResumeTime         time.Time            `json:"last_resume_time"`         // Timestamp of latest recovery after shutdown
	Leverage               DynamicLeverageState `json:"leverage"`                 // Real-time automatic dynamic leverage and regime
}

// DryRunEngine orchestrates the 14-day autonomous dry run.
type DryRunEngine struct {
	cfg     DryRunConfig
	ring    *engine.SPSCRing
	stream  *MarketStreamIngestor
	journal *os.File

	mu    sync.RWMutex
	state DryRunState

	// Resting passive maker orders per symbol
	activeOrders map[string][]*ActiveMakerOrder
	lastTickTime time.Time

	// Models & Quantitative guards
	conformal     *common.SplitConformalCalibrator
	glftMM        *common.GLFTMarketMaker
	carteaMM      *common.CarteaJaimungalMM
	adaHedge      *common.AdaHedgeEnsemble
	circuitBrk    *experiment.MicrostructureCircuitBreaker
	driftGuard    *experiment.PageHinkleyDriftGuard
	vpinCalc      *common.VPINCalculator
	riskSafeguard *CircuitBreaker
	kyleEst       *common.KylesLambdaEstimator
	tcaTracker    *TCAMarkoutTracker
	sor           common.SmartOrderRouter
	hawkesEst     *common.OnlineHawkesEstimator
	leverageCtrl  *DynamicLeverageController

	// Online tick tracking
	recentFills []TradeRecord
	running     atomic.Bool
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// NewDryRunEngine constructs and recovers or initializes the paper trading engine.
func NewDryRunEngine(cfg DryRunConfig, ring *engine.SPSCRing, stream *MarketStreamIngestor) (*DryRunEngine, error) {
	if cfg.InitialCapital <= 0 {
		cfg.InitialCapital = 1000.00
	}
	switch strings.ToLower(strings.TrimSpace(cfg.WindowPreset)) {
	case "7d", "1w", "week", "last_week", "last-week":
		cfg.TargetDuration = 7 * 24 * time.Hour
		cfg.WindowPreset = "7d"
	case "30d", "1m", "month", "last_month", "last-month":
		cfg.TargetDuration = 30 * 24 * time.Hour
		cfg.WindowPreset = "30d"
	case "14d", "2w", "2week", "two_weeks":
		cfg.TargetDuration = 14 * 24 * time.Hour
		cfg.WindowPreset = "14d"
	default:
		if cfg.TargetDuration <= 0 {
			cfg.TargetDuration = 14 * 24 * time.Hour
		}
		if cfg.WindowPreset == "" {
			if cfg.TargetDuration == 7*24*time.Hour {
				cfg.WindowPreset = "7d"
			} else if cfg.TargetDuration == 30*24*time.Hour {
				cfg.WindowPreset = "30d"
			} else {
				cfg.WindowPreset = "14d"
			}
		}
	}
	if cfg.MaxDrawdownPct <= 0 {
		cfg.MaxDrawdownPct = 2.0
	}
	if len(cfg.Symbols) == 0 {
		cfg.Symbols = []string{"BTCUSDT", "ETHUSDT"}
	}
	if cfg.KylesLambda <= 0 {
		cfg.KylesLambda = 0.25
	}
	if cfg.CheckpointFreq <= 0 {
		cfg.CheckpointFreq = 5 * time.Second
	}

	if cfg.OrderLatency <= 0 {
		cfg.OrderLatency = 50 * time.Millisecond
	}

	levCfg := DefaultDynamicLeverageConfig()
	levCfg.MaxDrawdownPct = cfg.MaxDrawdownPct
	levCtrl := NewDynamicLeverageController(levCfg)

	e := &DryRunEngine{
		cfg:           cfg,
		ring:          ring,
		stream:        stream,
		stopCh:        make(chan struct{}),
		conformal:     common.NewSplitConformalCalibrator(0.10),
		glftMM:        common.NewGLFTMarketMaker(1e-3, 0.05, cfg.MakerFeeBps),
		carteaMM:      common.NewCarteaJaimungalMM(0.05, 1.5, 2.0, 0.001, 0.01, 0.5, 60.0),
		adaHedge:      common.NewAdaHedgeEnsemble(7, 0.985),
		circuitBrk:    experiment.NewMicrostructureCircuitBreaker(1.0),
		driftGuard:    experiment.NewPageHinkleyDriftGuard(0.15, 12.0),
		vpinCalc:      common.NewVPINCalculator(5.0, 50),
		riskSafeguard: NewCircuitBreaker(DefaultCircuitBreakerConfig()),
		kyleEst:       common.NewKylesLambdaEstimator(0.05, cfg.KylesLambda),
		tcaTracker:    NewTCAMarkoutTracker(),
		sor:           common.NewSmartOrderRouter(),
		hawkesEst:     common.NewOnlineHawkesEstimator(5.0, 12.0, 20.0),
		leverageCtrl:  levCtrl,
		activeOrders:  make(map[string][]*ActiveMakerOrder),
		recentFills:   make([]TradeRecord, 0, 32),
	}

	// Attempt to restore existing state from checkpoint
	if err := e.loadCheckpoint(); err != nil {
		// Initialize fresh state if no checkpoint exists
		now := time.Now()
		targetDays := int(cfg.TargetDuration.Hours() / 24.0)
		if targetDays <= 0 {
			targetDays = 14
		}
		e.state = DryRunState{
			StartTime:      now,
			TargetEndTime:  now.Add(cfg.TargetDuration),
			LastCheckpoint: now,
			InitialCapital: cfg.InitialCapital,
			Cash:           cfg.InitialCapital,
			Equity:         cfg.InitialCapital,
			PeakEquity:     cfg.InitialCapital,
			Positions:      make(map[string]*Position, len(cfg.Symbols)),
			DailyReturns:   make([]float64, 0, targetDays),
			TargetDays:     targetDays,
			WindowPreset:   cfg.WindowPreset,
			DataSource:     "live",
			Leverage:       levCtrl.Compute(10.0, 0.20, 0.50, 0.0, false),
		}
		for _, s := range cfg.Symbols {
			upper := strings.ToUpper(s)
			e.state.Positions[upper] = &Position{
				Symbol: upper,
			}
		}
	} else {
		if e.state.DataSource == "" {
			e.state.DataSource = "live"
		}
		if e.state.TargetDays <= 0 {
			e.state.TargetDays = int(cfg.TargetDuration.Hours() / 24.0)
			if e.state.TargetDays <= 0 {
				e.state.TargetDays = 14
			}
		}
		if e.state.WindowPreset == "" {
			e.state.WindowPreset = cfg.WindowPreset
		}
	}

	// Ensure journal directory exists
	if cfg.JournalFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.JournalFile), 0o755)
		f, err := os.OpenFile(cfg.JournalFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("failed to open dry run journal %s: %w", cfg.JournalFile, err)
		}
		e.journal = f
	}

	return e, nil
}

// Start launches the strategy evaluation loop and periodic state checkpointing.
func (e *DryRunEngine) Start(ctx context.Context) {
	if e.running.Swap(true) {
		return
	}

	e.wg.Add(2)
	go e.runExecutionLoop(ctx)
	go e.runCheckpointLoop(ctx)
}

// Stop terminates the engine, cancels all resting orders, optionally auto-liquidates open inventory to cash, and checkpoints state.
func (e *DryRunEngine) Stop() {
	if !e.running.Swap(false) {
		return
	}
	close(e.stopCh)
	e.wg.Wait()

	e.mu.Lock()
	// 1. Cancel all resting maker orders on shutdown
	for s := range e.activeOrders {
		e.activeOrders[s] = nil
	}

	// 2. Auto TS (Trailing Stop / Liquidation) on Shutdown:
	// If configured (default true), close any open inventory at current MarkPrice to convert all unrealized PnL to cash
	if e.cfg.FlattenOnShutdown {
		for sym, pos := range e.state.Positions {
			if pos != nil && pos.Size != 0 && pos.MarkPrice > 0 {
				closePrice := pos.MarkPrice
				qty := math.Abs(pos.Size)
				valUSDT := closePrice * qty
				realizedPnL := 0.0

				if pos.Size > 0 { // Long position liquidated
					realizedPnL = pos.Size * (closePrice - pos.AvgEntryPrice)
					e.state.Cash += valUSDT
				} else { // Short position covered
					realizedPnL = (-pos.Size) * (pos.AvgEntryPrice - closePrice)
					e.state.Cash -= valUSDT
				}

				pos.RealizedPnL += realizedPnL
				pos.UnrealizedPnL = 0
				pos.Size = 0
				pos.TotalTrades++

				e.state.RealizedPnL += realizedPnL
				e.state.TotalTrades++
				if realizedPnL > 0 {
					e.state.WinningTrades++
				} else if realizedPnL < 0 {
					e.state.LosingTrades++
				}

				// Record liquidation fill in journal/blotter
				fillRec := TradeRecord{
					Timestamp:   time.Now().UTC(),
					Symbol:      sym,
					Side:        "SHUTDOWN_AUTO_LIQUIDATE",
					Role:        "TAKER_SAFETY",
					Price:       closePrice,
					Size:        qty,
					ValueUSDT:   valUSDT,
					FeeUSDT:     0.0,
					EdgeBps:     0.0,
					SlippageBps: 0.0,
					PnLRealized: realizedPnL,
					Strategy:    "Shutdown_Auto_TS_Flatten",
					DataSource:  "live",
				}
				e.recentFills = append([]TradeRecord{fillRec}, e.recentFills...)
				if len(e.recentFills) > 64 {
					e.recentFills = e.recentFills[:64]
				}
				if e.journal != nil {
					if b, err := json.Marshal(fillRec); err == nil {
						_, _ = e.journal.Write(append(b, '\n'))
					}
				}
			}
		}
		// Reset unrealized PnL and lock equity into cash
		e.state.UnrealizedPnL = 0
		e.state.Equity = e.state.Cash
	}
	e.mu.Unlock()

	_ = e.saveCheckpoint()
	if e.journal != nil {
		_ = e.journal.Close()
	}
}

// State returns a thread-safe snapshot of the current 14-day dry run state.
func (e *DryRunEngine) State() DryRunState {
	e.mu.RLock()
	defer e.mu.RUnlock()

	cp := e.state
	// Deep copy positions
	cp.Positions = make(map[string]*Position, len(e.state.Positions))
	for k, v := range e.state.Positions {
		posCopy := *v
		cp.Positions[k] = &posCopy
	}
	cp.DailyReturns = append([]float64(nil), e.state.DailyReturns...)

	// Update computed timing fields
	now := time.Now()
	elapsed := now.Sub(cp.StartTime)
	totalDays := cp.TargetDays
	if totalDays <= 0 {
		totalDays = int(e.cfg.TargetDuration.Hours() / 24.0)
		if totalDays <= 0 {
			totalDays = 14
		}
		cp.TargetDays = totalDays
	}
	if cp.WindowPreset == "" {
		cp.WindowPreset = e.cfg.WindowPreset
	}
	cp.DaysElapsed = int(elapsed.Hours()/24.0) + 1
	if cp.DaysElapsed > totalDays {
		cp.DaysElapsed = totalDays
	}
	remaining := cp.TargetEndTime.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	cp.HoursRemaining = remaining.Hours()

	if cp.Leverage.EffectiveLeverage == 0 && e.leverageCtrl != nil {
		cp.Leverage = e.leverageCtrl.Compute(10.0, 0.20, 0.50, 0.0, cp.CircuitHalted)
	}

	return cp
}

// RecentFills returns the most recent executed trades for the blotter.
func (e *DryRunEngine) RecentFills() []TradeRecord {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]TradeRecord, len(e.recentFills))
	copy(out, e.recentFills)
	return out
}

// StreamStats returns real-time event counts from the public WebSocket ingestor.
func (e *DryRunEngine) StreamStats() (total, depths, trades, gaps, reconnects, fallbacks uint64, lastNs int64) {
	if e.stream != nil {
		return e.stream.Stats()
	}
	return 0, 0, 0, 0, 0, 0, 0
}

// runCheckpointLoop periodically persists state to dryrun_state.json.
func (e *DryRunEngine) runCheckpointLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.CheckpointFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
			_ = e.saveCheckpoint()
		}
	}
}

// saveCheckpoint serializes DryRunState to JSON.
func (e *DryRunEngine) saveCheckpoint() error {
	e.mu.Lock()
	now := time.Now()
	e.state.LastCheckpoint = now

	// Protect against backward time jump before NTP sync on Raspberry Pi
	if !e.state.StartTime.IsZero() && now.After(e.state.StartTime) {
		elapsed := now.Sub(e.state.StartTime)
		e.state.DaysElapsed = int(elapsed.Hours()/24.0) + 1
		if e.state.DaysElapsed > 14 {
			e.state.DaysElapsed = 14
		}
		rem := e.state.TargetEndTime.Sub(now)
		if rem < 0 {
			rem = 0
		}
		e.state.HoursRemaining = rem.Hours()
	}

	data, err := json.MarshalIndent(e.state, "", "  ")
	e.mu.Unlock()
	if err != nil {
		return err
	}

	if e.cfg.StateFile == "" {
		return nil
	}

	_ = os.MkdirAll(filepath.Dir(e.cfg.StateFile), 0o755)
	tmpFile := e.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpFile, e.cfg.StateFile)
}

// loadCheckpoint deserializes state from JSON and handles shutdown gap recovery.
func (e *DryRunEngine) loadCheckpoint() error {
	if e.cfg.StateFile == "" {
		return os.ErrNotExist
	}
	data, err := os.ReadFile(e.cfg.StateFile)
	if err != nil {
		return err
	}

	var st DryRunState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}

	if st.Positions == nil {
		st.Positions = make(map[string]*Position)
	}

	now := time.Now()
	// Detect shutdown gap and recover seamlessly
	if !st.LastCheckpoint.IsZero() {
		if now.Before(st.LastCheckpoint) {
			// System clock warped backwards (Raspberry Pi without RTC booted before NTP sync).
			// Keep existing target end time and hours remaining intact until NTP catches up.
		} else {
			offlineGap := now.Sub(st.LastCheckpoint)
			// If offline for more than 30s (e.g. 12-hour power-off, reboot, or transport):
			if offlineGap > 30*time.Second {
				st.OfflineDowntimeSeconds += offlineGap.Seconds()
				st.FeedDownSeconds += offlineGap.Seconds()
				st.ShutdownCount++
				st.LastResumeTime = now

				// If configured (default true), extend target end time by the offline duration
				// so the user gets the FULL 336 hours of active live market evaluation!
				if e.cfg.ExtendOnShutdown && !st.TargetEndTime.IsZero() {
					st.TargetEndTime = st.TargetEndTime.Add(offlineGap)
				}
				rem := st.TargetEndTime.Sub(now)
				if rem < 0 {
					rem = 0
				}
				st.HoursRemaining = rem.Hours()
			}
		}
	}

	e.mu.Lock()
	e.state = st
	e.mu.Unlock()
	return nil
}

// runExecutionLoop consumes MarketEvents from SPSCRing and evaluates quantitative signals.
func (e *DryRunEngine) runExecutionLoop(ctx context.Context) {
	defer e.wg.Done()

	var eventCount uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		default:
		}

		ev, ok := e.ring.Pop()
		if !ok {
			// Ring is empty; yield processor briefly
			time.Sleep(50 * time.Microsecond)
			continue
		}

		eventCount++
		e.processMarketEvent(&ev)
	}
}

// ProcessMarketEvent processes an individual market event through pricing and order matching.
func (e *DryRunEngine) ProcessMarketEvent(ev *engine.MarketEvent) {
	e.processMarketEvent(ev)
}

// processMarketEvent executes hot-path pricing and order matching.
func (e *DryRunEngine) processMarketEvent(ev *engine.MarketEvent) {
	symbol := "BTCUSDT"
	if ev.SymbolID == 1 {
		symbol = "ETHUSDT"
	}

	price := ev.Price
	if price <= 0 {
		return
	}

	now := time.Now()

	// Check feed liveness & fallback status
	isFallback := false
	if e.stream != nil && e.stream.IsFallback() {
		isFallback = true
	}

	e.mu.Lock()
	if isFallback {
		e.state.DataSource = "fallback"
		e.state.QuotingPaused = true
		if !e.lastTickTime.IsZero() {
			diff := now.Sub(e.lastTickTime).Seconds()
			if diff > 0 && diff < 60 {
				e.state.FeedDownSeconds += diff
			}
		}
		e.lastTickTime = now
		e.mu.Unlock()
		// In dry-run mode, synthetic fallback must NEVER drive paper PnL:
		// while on fallback, pause quoting/fills, and keep marks frozen.
		return
	}

	e.state.DataSource = "live"
	e.state.QuotingPaused = false
	if !e.lastTickTime.IsZero() {
		// When transitioning back to live, time tracking continues
	}
	e.lastTickTime = now

	// 1. Update mark price and mark-to-market equity for live ticks
	pos, ok := e.state.Positions[symbol]
	if !ok {
		pos = &Position{Symbol: symbol}
		e.state.Positions[symbol] = pos
	}
	pos.MarkPrice = price
	if pos.Size != 0 {
		pos.UnrealizedPnL = pos.Size * (price - pos.AvgEntryPrice)
	} else {
		pos.UnrealizedPnL = 0
	}

	// Recompute total portfolio equity
	var totalUnrealized float64
	for _, p := range e.state.Positions {
		totalUnrealized += p.UnrealizedPnL
	}
	e.state.UnrealizedPnL = totalUnrealized
	e.state.Equity = e.state.Cash + totalUnrealized

	if e.state.Equity > e.state.PeakEquity {
		e.state.PeakEquity = e.state.Equity
	}

	// Check max drawdown
	if e.state.PeakEquity > 0 {
		dd := (e.state.PeakEquity - e.state.Equity) / e.state.PeakEquity * 100.0
		if dd > e.state.MaxDrawdownPct {
			e.state.MaxDrawdownPct = dd
		}
		if dd >= e.cfg.MaxDrawdownPct && !e.state.CircuitHalted {
			e.state.CircuitHalted = true
			e.state.CircuitHaltReason = fmt.Sprintf("Emergency Circuit Breaker: Max Drawdown breached (%.2f%% >= %.2f%%)", dd, e.cfg.MaxDrawdownPct)
		}
	}

	if e.state.InitialCapital > 0 {
		e.state.TotalReturnPct = (e.state.Equity - e.state.InitialCapital) / e.state.InitialCapital * 100.0
	}
	e.mu.Unlock()

	// Update TCA multi-horizon markouts on incoming live market tick
	if e.tcaTracker != nil {
		e.tcaTracker.OnTick(symbol, price, now.UnixNano())
	}

	// 2. Microstructure Circuit Breaker Check
	if e.state.CircuitHalted {
		return
	}

	// 3. Trade event (ev.Kind == 0):
	// Realistic order fill matching:
	// A resting maker order fills only when observed trades at or through its price
	// consume the queue ahead (hftbacktest fill realism).
	if ev.Kind == 0 {
		isBuyerMaker := (ev.Side == 1)
		if e.vpinCalc != nil {
			e.vpinCalc.UpdateTrade(price, ev.Size, isBuyerMaker)
		}
		if e.kyleEst != nil {
			e.kyleEst.UpdateTrade(price, ev.Size, isBuyerMaker)
		}
		if e.hawkesEst != nil {
			e.hawkesEst.Update(ev.Timestamp, !isBuyerMaker, ev.Size)
		}
		e.matchMakerOrders(symbol, price, ev.Size, ev.Side, now)
		return
	}

	// 4. Depth update event (ev.Kind == 1):
	// Update order book metrics, evaluate queue dynamics and maintain resting quotes
	e.evaluateQuotesAndGating(symbol, price, now)
}

// matchMakerOrders matches active resting maker orders against observed trade prints.
// A maker order fills only when trades at or through its price deplete the queue ahead.
func (e *DryRunEngine) matchMakerOrders(symbol string, tradePrice float64, tradeSize float64, tradeSide uint8, now time.Time) {
	e.mu.Lock()
	orders := e.activeOrders[symbol]
	if len(orders) == 0 {
		e.mu.Unlock()
		return
	}

	var remaining []*ActiveMakerOrder
	for _, ord := range orders {
		// Feed + order latency: resting order does not participate in matching before ActiveAt
		if now.Before(ord.ActiveAt) {
			remaining = append(remaining, ord)
			continue
		}

		filled := false
		if ord.Side == "BUY" {
			// BUY bid order fills if market trade sweeps through our price or consumes queue at our price
			if tradePrice < ord.Price {
				filled = true
			} else if math.Abs(tradePrice-ord.Price) <= math.Max(0.50, ord.Price*1e-4) {
				ord.QueueAhead -= tradeSize
				if ord.QueueAhead <= 0 {
					filled = true
				}
			}
		} else if ord.Side == "SELL" {
			// SELL ask order fills if market trade sweeps through our price or consumes queue at our price
			if tradePrice > ord.Price {
				filled = true
			} else if math.Abs(tradePrice-ord.Price) <= math.Max(0.50, ord.Price*1e-4) {
				ord.QueueAhead -= tradeSize
				if ord.QueueAhead <= 0 {
					filled = true
				}
			}
		}

		if filled {
			e.executeMakerFillLocked(symbol, ord.Price, ord.Size, ord.Side, ord.EdgeBps, ord.Strategy)
		} else {
			remaining = append(remaining, ord)
		}
	}
	e.activeOrders[symbol] = remaining
	e.mu.Unlock()
}

// evaluateQuotesAndGating calculates order book signals and maintains resting maker quotes.
func (e *DryRunEngine) evaluateQuotesAndGating(symbol string, midPrice float64, now time.Time) {
	var spreadBps float64 = 0.80
	var top5Depth float64 = 150000.0
	var imbL1 float64 = 0.20
	var queueRatio float64 = 0.55
	var bestBid float64 = midPrice * 0.9999
	var bestAsk float64 = midPrice * 1.0001
	var bidSize float64 = 1.5
	var askSize float64 = 1.5

	if e.stream != nil {
		if book, hasBook := e.stream.GetL2Book(symbol); hasBook && book != nil && book.NumBids > 0 && book.NumAsks > 0 {
			b0 := book.Bids[0]
			a0 := book.Asks[0]
			bestBid = b0.Price
			bestAsk = a0.Price
			bidSize = b0.Size
			askSize = a0.Size
			if bestAsk > bestBid && midPrice > 0 {
				spreadBps = (bestAsk - bestBid) / midPrice * 1e4
			}
			totSize := b0.Size + a0.Size
			if totSize > 0 {
				imbL1 = (b0.Size - a0.Size) / totSize
				queueRatio = b0.Size / totSize
			}
			var sumTop5 float64
			for i := 0; i < 5 && i < book.NumBids; i++ {
				sumTop5 += book.Bids[i].Size * book.Bids[i].Price
			}
			for i := 0; i < 5 && i < book.NumAsks; i++ {
				sumTop5 += book.Asks[i].Size * book.Asks[i].Price
			}
			if sumTop5 > 0 {
				top5Depth = sumTop5
			}
		}
	}

	// Microstructure Circuit Breaker & Dynamic Risk Safeguard Check
	liveVpin := e.vpinCalc.VPIN()
	halted, haltReason := e.circuitBrk.Check(spreadBps, top5Depth, liveVpin)
	if e.riskSafeguard != nil {
		rState, rReason := e.riskSafeguard.OnTick(e.state.Equity, spreadBps, liveVpin, now)
		if rState == StateTripped {
			halted = true
			haltReason = rReason
		}
	}
	if halted {
		e.mu.Lock()
		e.state.CircuitHalted = true
		e.state.CircuitHaltReason = haltReason
		e.activeOrders[symbol] = nil // Cancel all resting quotes on halt
		e.mu.Unlock()
		return
	}

	// Synthesize standard feature vector for current tick
	var x [common.FeatureCount]float64
	x[0] = imbL1
	x[3] = 0.10
	x[4] = spreadBps
	x[5] = 1.20
	x[10] = 1.50
	x[11] = 0.35
	x[13] = 0.25
	x[14] = 12.0
	x[15] = queueRatio

	e.mu.RLock()
	pos := e.state.Positions[symbol]
	currentInv := 0.0
	if pos != nil {
		currentInv = pos.Size
	}
	e.mu.RUnlock()

	// GLFT & Cartea-Jaimungal reservation prices
	qd := e.glftMM.Quote(&x, currentInv)
	_, _, carteaRes := e.carteaMM.ComputeQuotes(midPrice, currentInv, 0.0, 30.0, x[5])

	// AdaHedge ensemble with Ablation Gate filtering
	carteaSignal := 0.0
	if e.cfg.EnableAllModels || !experiment.DefaultDisabledModels["cartea_jaimungal"] {
		carteaSignal = (carteaRes - midPrice) / midPrice * 1e4
	}
	prevPreds := [common.MaxEnsembleExperts]float64{0.48, 0.42, 0.44, 0.46, 0.39, 0.51, 0.45, 0}
	currPreds := [common.MaxEnsembleExperts]float64{0.45, 0.38, qd.ReservationBps, 0.44, 0.40, 0.52, carteaSignal, 0}
	metaPred, _ := e.adaHedge.UpdateAndCombine(0.50, prevPreds, currPreds)

	// Target quotes
	targetBid := bestBid
	if targetBid <= 0 || targetBid >= midPrice {
		targetBid = midPrice - (qd.BidOffsetBps * midPrice * 1e-4)
	}
	targetAsk := bestAsk
	if targetAsk <= 0 || targetAsk <= midPrice {
		targetAsk = midPrice + (qd.AskOffsetBps * midPrice * 1e-4)
	}

	// Dynamic Leverage Controller: automatic regime & exposure scaling
	hkRatio := 0.60
	if e.hawkesEst != nil {
		hkRatio = e.hawkesEst.BranchingRatio()
	}
	ddPct := 0.0
	if e.state.PeakEquity > 0 {
		ddPct = (e.state.PeakEquity - e.state.Equity) / e.state.PeakEquity * 100.0
	}
	levState := e.leverageCtrl.Compute(spreadBps, liveVpin, hkRatio, ddPct, e.state.CircuitHalted)
	e.state.Leverage = levState

	// Dynamic institutional order sizing bounded by effective leverage and capital (symbol-specific vol parity)
	qty := e.leverageCtrl.ComputeOrderSizeForSymbol(symbol, e.state.Equity, levState.EffectiveLeverage, midPrice, len(e.cfg.Symbols))
	if qty <= 0 {
		qty = 0.001
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	orders := e.activeOrders[symbol]
	var keptOrders []*ActiveMakerOrder
	hasBuy := false
	hasSell := false
	for _, ord := range orders {
		target := targetBid
		if ord.Side == "SELL" {
			target = targetAsk
		}
		// Cancel order if price deviated > 1.0 bps or if order has rested for > 2 seconds
		devBps := math.Abs(ord.Price-target) / target * 1e4
		if devBps > 1.0 || now.Sub(ord.PlacedAt) > 2*time.Second {
			continue // cancel stale order
		}
		keptOrders = append(keptOrders, ord)
		if ord.Side == "BUY" {
			hasBuy = true
		}
		if ord.Side == "SELL" {
			hasSell = true
		}
	}
	orders = keptOrders

	latency := e.cfg.OrderLatency
	if latency <= 0 {
		latency = 50 * time.Millisecond
	}

	// Place resting BUY maker quote if not present and inventory limit not exceeded
	if !hasBuy && currentInv < 5.0*qty {
		orders = append(orders, &ActiveMakerOrder{
			Symbol:     symbol,
			Side:       "BUY",
			Price:      targetBid,
			Size:       qty,
			PlacedAt:   now,
			ActiveAt:   now.Add(latency),
			QueueAhead: bidSize,
			Strategy:   "GLFT_QueueReactive_Maker",
			EdgeBps:    qd.HalfSpreadBps,
		})
	}

	// Place resting SELL maker quote if not present and inventory limit not exceeded
	if !hasSell && currentInv > -5.0*qty {
		orders = append(orders, &ActiveMakerOrder{
			Symbol:     symbol,
			Side:       "SELL",
			Price:      targetAsk,
			Size:       qty,
			PlacedAt:   now,
			ActiveAt:   now.Add(latency),
			QueueAhead: askSize,
			Strategy:   "GLFT_QueueReactive_Maker",
			EdgeBps:    qd.HalfSpreadBps,
		})
	}
	e.activeOrders[symbol] = orders

	// Tactical Taker Execution:
	// Only triggered on high-conviction directional forecast passing conformal gate against taker costs
	if math.Abs(metaPred) > 2.5 && e.conformal.PassesConformalGate(metaPred, x[10], e.cfg.TakerFeeBps) {
		e.simulateTakerFillLocked(symbol, midPrice, metaPred)
	}
}

// simulateMakerFill provides direct maker fill simulation for testing.
func (e *DryRunEngine) simulateMakerFill(symbol string, midPrice float64, qd common.QuoteDecision, edgeBps float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	side := "BUY"
	orderPrice := midPrice - (qd.BidOffsetBps * midPrice * 1e-4)
	effLev := 2.5
	if e.state.Leverage.EffectiveLeverage > 0 {
		effLev = e.state.Leverage.EffectiveLeverage
	}
	qty := 0.01
	if e.leverageCtrl != nil && midPrice > 0 {
		qty = e.leverageCtrl.ComputeOrderSize(e.state.Equity, effLev, midPrice, len(e.cfg.Symbols))
	}
	if qty <= 0 {
		qty = 0.001
	}
	if qd.ReservationBps < 0 {
		side = "SELL"
		orderPrice = midPrice + (qd.AskOffsetBps * midPrice * 1e-4)
	}

	e.executeMakerFillLocked(symbol, orderPrice, qty, side, edgeBps, "GLFT_Maker_Test")
}

// simulateTakerFill provides direct taker fill simulation for testing.
func (e *DryRunEngine) simulateTakerFill(symbol string, midPrice float64, alphaBps float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.simulateTakerFillLocked(symbol, midPrice, alphaBps)
}

// executeMakerFillLocked executes a passive maker order fill with spread capture. Caller must hold e.mu.
func (e *DryRunEngine) executeMakerFillLocked(symbol string, orderPrice, qty float64, side string, edgeBps float64, strategy string) {
	valUSDT := orderPrice * qty
	feeUSDT := valUSDT * (e.cfg.MakerFeeBps * 1e-4)

	pos := e.state.Positions[symbol]
	realizedPnL := 0.0

	if side == "BUY" {
		if pos.Size < 0 { // Closing short
			closeQty := math.Min(-pos.Size, qty)
			realizedPnL = closeQty * (pos.AvgEntryPrice - orderPrice)
		}
		newSize := pos.Size + qty
		if newSize > 0 {
			if pos.Size >= 0 {
				pos.AvgEntryPrice = (pos.AvgEntryPrice*pos.Size + orderPrice*qty) / newSize
			} else {
				pos.AvgEntryPrice = orderPrice
			}
		}
		pos.Size = newSize
		e.state.Cash -= (valUSDT + feeUSDT)
	} else {
		if pos.Size > 0 { // Closing long
			closeQty := math.Min(pos.Size, qty)
			realizedPnL = closeQty * (orderPrice - pos.AvgEntryPrice)
		}
		newSize := pos.Size - qty
		if newSize < 0 {
			if pos.Size <= 0 {
				pos.AvgEntryPrice = (pos.AvgEntryPrice*(-pos.Size) + orderPrice*qty) / (-newSize)
			} else {
				pos.AvgEntryPrice = orderPrice
			}
		}
		pos.Size = newSize
		e.state.Cash += (valUSDT - feeUSDT)
	}

	pos.RealizedPnL += realizedPnL
	pos.TotalTrades++
	pos.MakerTrades++
	pos.TotalVolumeUSDT += valUSDT
	if e.riskSafeguard != nil {
		e.riskSafeguard.OnFill(realizedPnL, pos.Size)
	}
	if e.tcaTracker != nil {
		sideInt := int8(1)
		if side == "SELL" {
			sideInt = -1
		}
		mid := pos.MarkPrice
		if mid <= 0 {
			mid = orderPrice
		}
		e.tcaTracker.RecordFill(uint64(e.state.TotalTrades), symbol, sideInt, orderPrice, mid, time.Now().UnixNano())
	}

	e.state.RealizedPnL += realizedPnL
	e.state.TotalTrades++
	if realizedPnL > 0 {
		e.state.WinningTrades++
	} else if realizedPnL < 0 {
		e.state.LosingTrades++
	}

	if e.state.TotalTrades > 0 {
		e.state.WinRate = float64(e.state.WinningTrades) / float64(e.state.TotalTrades) * 100.0
	}

	dataSource := e.state.DataSource
	if dataSource == "" {
		dataSource = "live"
	}

	rec := TradeRecord{
		Timestamp:   time.Now().UTC(),
		Symbol:      symbol,
		Side:        side,
		Role:        "MAKER",
		Price:       orderPrice,
		Size:        qty,
		ValueUSDT:   valUSDT,
		FeeUSDT:     feeUSDT,
		EdgeBps:     edgeBps,
		SlippageBps: 0.0,
		PnLRealized: realizedPnL,
		Strategy:    strategy,
		DataSource:  dataSource,
	}

	e.appendTradeRecord(rec)
}

// simulateTakerFillLocked simulates an aggressive taker order. Caller must hold e.mu.
func (e *DryRunEngine) simulateTakerFillLocked(symbol string, midPrice float64, alphaBps float64) {
	side := "BUY"
	effLev := 2.5
	if e.state.Leverage.EffectiveLeverage > 0 {
		effLev = e.state.Leverage.EffectiveLeverage
	}
	qty := 0.005
	if e.leverageCtrl != nil && midPrice > 0 {
		qty = e.leverageCtrl.ComputeOrderSize(e.state.Equity, effLev*0.5, midPrice, len(e.cfg.Symbols))
	}
	if qty <= 0 {
		qty = 0.001
	}
	if alphaBps < 0 {
		side = "SELL"
	}

	// Kyle's lambda slippage: slippage = lambda * sqrt(order_volume)
	slippageBps := e.cfg.KylesLambda * math.Sqrt(qty)
	execPrice := midPrice * (1.0 + slippageBps*1e-4)
	if side == "SELL" {
		execPrice = midPrice * (1.0 - slippageBps*1e-4)
	}

	valUSDT := execPrice * qty
	feeUSDT := valUSDT * (e.cfg.TakerFeeBps * 1e-4)

	pos := e.state.Positions[symbol]
	realizedPnL := 0.0

	if side == "BUY" {
		if pos.Size < 0 {
			closeQty := math.Min(-pos.Size, qty)
			realizedPnL = closeQty * (pos.AvgEntryPrice - execPrice)
		}
		newSize := pos.Size + qty
		if newSize > 0 {
			if pos.Size >= 0 {
				pos.AvgEntryPrice = (pos.AvgEntryPrice*pos.Size + execPrice*qty) / newSize
			} else {
				pos.AvgEntryPrice = execPrice
			}
		}
		pos.Size = newSize
		e.state.Cash -= (valUSDT + feeUSDT)
	} else {
		if pos.Size > 0 {
			closeQty := math.Min(pos.Size, qty)
			realizedPnL = closeQty * (execPrice - pos.AvgEntryPrice)
		}
		newSize := pos.Size - qty
		if newSize < 0 {
			if pos.Size <= 0 {
				pos.AvgEntryPrice = (pos.AvgEntryPrice*(-pos.Size) + execPrice*qty) / (-newSize)
			} else {
				pos.AvgEntryPrice = execPrice
			}
		}
		pos.Size = newSize
		e.state.Cash += (valUSDT - feeUSDT)
	}

	pos.RealizedPnL += realizedPnL
	pos.TotalTrades++
	pos.TakerTrades++
	pos.TotalVolumeUSDT += valUSDT
	if e.riskSafeguard != nil {
		e.riskSafeguard.OnFill(realizedPnL, pos.Size)
	}
	if e.tcaTracker != nil {
		sideInt := int8(1)
		if side == "SELL" {
			sideInt = -1
		}
		e.tcaTracker.RecordFill(uint64(e.state.TotalTrades), symbol, sideInt, execPrice, midPrice, time.Now().UnixNano())
	}

	e.state.RealizedPnL += realizedPnL
	e.state.TotalTrades++
	if realizedPnL > 0 {
		e.state.WinningTrades++
	} else if realizedPnL < 0 {
		e.state.LosingTrades++
	}

	if e.state.TotalTrades > 0 {
		e.state.WinRate = float64(e.state.WinningTrades) / float64(e.state.TotalTrades) * 100.0
	}

	dataSource := e.state.DataSource
	if dataSource == "" {
		dataSource = "live"
	}

	rec := TradeRecord{
		Timestamp:   time.Now().UTC(),
		Symbol:      symbol,
		Side:        side,
		Role:        "TAKER",
		Price:       execPrice,
		Size:        qty,
		ValueUSDT:   valUSDT,
		FeeUSDT:     feeUSDT,
		EdgeBps:     alphaBps,
		SlippageBps: slippageBps,
		PnLRealized: realizedPnL,
		Strategy:    "AdaHedge_Conformal_Taker",
		DataSource:  dataSource,
	}

	e.appendTradeRecord(rec)
}

func (e *DryRunEngine) appendTradeRecord(rec TradeRecord) {
	if len(e.recentFills) >= 30 {
		e.recentFills = e.recentFills[1:]
	}
	e.recentFills = append(e.recentFills, rec)

	// Append to JSONL journal
	if e.journal != nil {
		line, err := json.Marshal(rec)
		if err == nil {
			_, _ = e.journal.Write(append(line, '\n'))
		}
	}
}

// RiskSafeguard returns the dynamic circuit breaker engine.
func (e *DryRunEngine) RiskSafeguard() *CircuitBreaker {
	return e.riskSafeguard
}

// KylesLambda returns the real-time Kyle's Lambda estimator.
func (e *DryRunEngine) KylesLambda() *common.KylesLambdaEstimator {
	return e.kyleEst
}

// VPIN returns the online VPIN toxicity estimator.
func (e *DryRunEngine) VPIN() *common.VPINCalculator {
	return e.vpinCalc
}

// TCAMarkout returns the institutional post-trade transaction cost analysis tracker.
func (e *DryRunEngine) TCAMarkout() *TCAMarkoutTracker {
	return e.tcaTracker
}
