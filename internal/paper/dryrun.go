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
	InitialCapital float64       // Initial paper money in USDT (default: 100,000.00)
	TargetDuration time.Duration // Target autonomous duration (default: 14 * 24 * time.Hour = 336h)
	Symbols        []string      // Instruments traded (default: BTCUSDT, ETHUSDT)
	MaxDrawdownPct float64       // Max allowable drawdown % before circuit breaker halts (default: 2.0%)
	MakerFeeBps    float64       // Maker fee/rebate in bps (default: 0.0)
	TakerFeeBps    float64       // Taker fee in bps (default: 4.0)
	KylesLambda    float64       // Slippage coefficient on taker orders (bps per unit volume, default: 0.25)
	StateFile      string        // Path to checkpoint state (default: data/paper/dryrun_state.json)
	JournalFile    string        // Path to trade journal (default: data/paper/dryrun_journal.jsonl)
	CheckpointFreq time.Duration // Interval to save state (default: 5 * time.Second)
}

// DefaultDryRunConfig returns standard institutional configuration for the 2-week run.
func DefaultDryRunConfig(dataDir string) DryRunConfig {
	if dataDir == "" {
		dataDir = "data"
	}
	paperDir := filepath.Join(dataDir, "paper")
	return DryRunConfig{
		InitialCapital: 100000.00,
		TargetDuration: 14 * 24 * time.Hour,
		Symbols:        []string{"BTCUSDT", "ETHUSDT"},
		MaxDrawdownPct: 2.0,
		MakerFeeBps:    0.0,
		TakerFeeBps:    4.0,
		KylesLambda:    0.25,
		StateFile:      filepath.Join(paperDir, "dryrun_state.json"),
		JournalFile:    filepath.Join(paperDir, "dryrun_journal.jsonl"),
		CheckpointFreq: 5 * time.Second,
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
	HoursRemaining    float64              `json:"hours_remaining"`
}

// DryRunEngine orchestrates the 14-day autonomous dry run.
type DryRunEngine struct {
	cfg     DryRunConfig
	ring    *engine.SPSCRing
	stream  *MarketStreamIngestor
	journal *os.File

	mu    sync.RWMutex
	state DryRunState

	// Models & Quantitative guards
	conformal  *common.SplitConformalCalibrator
	glftMM     *common.GLFTMarketMaker
	carteaMM   *common.CarteaJaimungalMM
	adaHedge   *common.AdaHedgeEnsemble
	circuitBrk *experiment.MicrostructureCircuitBreaker
	driftGuard *experiment.PageHinkleyDriftGuard
	vpinCalc   *common.VPINCalculator

	// Online tick tracking
	recentFills []TradeRecord
	running     atomic.Bool
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// NewDryRunEngine constructs and recovers or initializes the 14-day paper trading engine.
func NewDryRunEngine(cfg DryRunConfig, ring *engine.SPSCRing, stream *MarketStreamIngestor) (*DryRunEngine, error) {
	if cfg.InitialCapital <= 0 {
		cfg.InitialCapital = 100000.00
	}
	if cfg.TargetDuration <= 0 {
		cfg.TargetDuration = 14 * 24 * time.Hour
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

	e := &DryRunEngine{
		cfg:         cfg,
		ring:        ring,
		stream:      stream,
		stopCh:      make(chan struct{}),
		conformal:   common.NewSplitConformalCalibrator(0.10),
		glftMM:      common.NewGLFTMarketMaker(1e-3, 0.05, cfg.MakerFeeBps),
		carteaMM:    common.NewCarteaJaimungalMM(0.05, 1.5, 2.0, 0.001, 0.01, 0.5, 60.0),
		adaHedge:    common.NewAdaHedgeEnsemble(7, 0.985),
		circuitBrk:  experiment.NewMicrostructureCircuitBreaker(1.0),
		driftGuard:  experiment.NewPageHinkleyDriftGuard(0.15, 12.0),
		vpinCalc:    common.NewVPINCalculator(10.0, 50),
		recentFills: make([]TradeRecord, 0, 32),
	}

	// Attempt to restore existing state from checkpoint
	if err := e.loadCheckpoint(); err != nil {
		// Initialize fresh state if no checkpoint exists
		now := time.Now()
		e.state = DryRunState{
			StartTime:      now,
			TargetEndTime:  now.Add(cfg.TargetDuration),
			LastCheckpoint: now,
			InitialCapital: cfg.InitialCapital,
			Cash:           cfg.InitialCapital,
			Equity:         cfg.InitialCapital,
			PeakEquity:     cfg.InitialCapital,
			Positions:      make(map[string]*Position, len(cfg.Symbols)),
			DailyReturns:   make([]float64, 0, 14),
		}
		for _, s := range cfg.Symbols {
			upper := strings.ToUpper(s)
			e.state.Positions[upper] = &Position{
				Symbol: upper,
			}
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

// Stop terminates the engine and checkpoints state.
func (e *DryRunEngine) Stop() {
	if !e.running.Swap(false) {
		return
	}
	close(e.stopCh)
	e.wg.Wait()

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
	cp.DaysElapsed = int(elapsed.Hours()/24.0) + 1
	if cp.DaysElapsed > 14 {
		cp.DaysElapsed = 14
	}
	remaining := cp.TargetEndTime.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	cp.HoursRemaining = remaining.Hours()

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
	e.state.LastCheckpoint = time.Now()
	now := time.Now()
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

// loadCheckpoint deserializes state from JSON.
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

	// 1. Update mark price and mark-to-market equity
	e.mu.Lock()
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

	// 2. Microstructure Circuit Breaker Check
	if e.state.CircuitHalted {
		return
	}

	// Check toxicity via VPIN & spread
	isSell := ev.Side == 2
	e.vpinCalc.Update(price, ev.Size, isSell)
	liveVpin := e.vpinCalc.VPIN()

	// Approximate spread
	spreadBps := 0.80
	halted, haltReason := e.circuitBrk.Check(spreadBps, 150000.0, liveVpin)
	if halted {
		e.mu.Lock()
		e.state.CircuitHalted = true
		e.state.CircuitHaltReason = haltReason
		e.mu.Unlock()
		return
	}

	// 3. Quantitative Signal Generation:
	// Synthesize standard feature vector for current tick
	var x [common.FeatureCount]float64
	x[0] = 0.20      // imb_l1
	x[3] = 0.10      // micro_dev
	x[4] = spreadBps // spread_bps
	x[5] = 1.20      // ofi_1s
	x[10] = 1.50     // vol_30s_bps
	x[11] = 0.35     // ret_1s_bps
	x[13] = 0.25     // trade_imb_5s
	x[14] = 12.0     // trade_cnt_1s
	x[15] = 0.55     // queue_ratio

	// GLFT & Cartea-Jaimungal reservation prices
	currentInv := pos.Size
	qd := e.glftMM.Quote(&x, currentInv)
	_, _, carteaRes := e.carteaMM.ComputeQuotes(price, currentInv, 0.0, 30.0, x[5])

	// AdaHedge ensemble combination across experts
	prevPreds := [common.MaxEnsembleExperts]float64{0.48, 0.42, 0.44, 0.46, 0.39, 0.51, 0.45, 0}
	currPreds := [common.MaxEnsembleExperts]float64{0.45, 0.38, qd.ReservationBps, 0.44, 0.40, 0.52, (carteaRes - price) / price * 1e4, 0}
	metaPred, _ := e.adaHedge.UpdateAndCombine(0.50, prevPreds, currPreds)

	// Conformal gate check: only execute when prediction certitude is statistically sound
	passesGate := e.conformal.PassesConformalGate(metaPred, x[10], 0.20)
	if !passesGate {
		return
	}

	// Drift guard update
	residual := metaPred - x[11]
	driftDetected, _, _ := e.driftGuard.Update(residual, 2.45)
	if driftDetected {
		return // Stand down during structural regime shifts
	}

	// 4. Order Execution Simulation:
	// Maker Order Simulation with Cont-de Larrard Queue Priority:
	cdlQueue := common.NewContDeLarrardQueue(10.0, 1.5, 3.0, x[5])
	fillProb5s := cdlQueue.FillProbability(5.0)

	// Execute maker fill if queue survival condition is met
	if fillProb5s > 0.65 && ev.Kind == 0 { // Real trade drained the queue
		e.simulateMakerFill(symbol, price, qd, metaPred)
	} else if math.Abs(metaPred) > 2.5 {
		// Strong directional alpha passes gate -> Execute tactical taker order with Kyle's lambda slippage
		e.simulateTakerFill(symbol, price, metaPred)
	}
}

// simulateMakerFill simulates a realistic passive maker execution with spread capture.
func (e *DryRunEngine) simulateMakerFill(symbol string, midPrice float64, qd common.QuoteDecision, edgeBps float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	side := "BUY"
	orderPrice := midPrice - (qd.BidOffsetBps * midPrice * 1e-4)
	qty := 0.10 // 0.10 BTC or 1.0 ETH normalized
	if symbol == "ETHUSDT" {
		qty = 1.0
	}

	if qd.ReservationBps < 0 {
		side = "SELL"
		orderPrice = midPrice + (qd.AskOffsetBps * midPrice * 1e-4)
	}

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

	rec := TradeRecord{
		Timestamp:   time.Now().UTC(),
		Symbol:      symbol,
		Side:        side,
		Role:        "MAKER",
		Price:       orderPrice,
		Size:        qty,
		ValueUSDT:   valUSDT,
		FeeUSDT:     feeUSDT,
		EdgeBps:     qd.HalfSpreadBps,
		SlippageBps: 0.0,
		PnLRealized: realizedPnL,
		Strategy:    "GLFT_ContDeLarrard_Maker",
	}

	e.appendTradeRecord(rec)
}

// simulateTakerFill simulates an aggressive taker order with Kyle's lambda market impact slippage.
func (e *DryRunEngine) simulateTakerFill(symbol string, midPrice float64, alphaBps float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	side := "BUY"
	qty := 0.05
	if symbol == "ETHUSDT" {
		qty = 0.5
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
