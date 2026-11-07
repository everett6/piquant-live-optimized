package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/dashboard"
	"piquant-live/internal/discovery"
	"piquant-live/internal/engine"
	"piquant-live/internal/experiment"
	"piquant-live/internal/paper"
	"piquant-live/internal/secrets"
)

type dryRunFlags struct {
	serveFlags
	capital           float64
	durationH         float64
	window            string
	maxDD             float64
	symbols           string
	statusOnly        bool
	reportOnly        bool
	enableFallback    bool
	enableAllModels   bool
	extendOnShutdown  bool
	flattenOnShutdown bool
	wsURL             string
}

func addDryRunFlags(fs *flag.FlagSet) *dryRunFlags {
	f := &dryRunFlags{}
	fs.StringVar(&f.addr, "addr", ":8080", "listen address for the monitoring dashboard, e.g. :8080")
	fs.StringVar(&f.secretsDir, "secrets-dir", "", "secrets directory (default ~/.config/piquant)")
	fs.StringVar(&f.dataDir, "data", "", "data directory where paper checkpoints live")
	fs.StringVar(&f.logDir, "log-dir", "", "directory holding the service logs")
	fs.BoolVar(&f.noDiscover, "no-discovery", false, "do not announce or search for the peer board")
	fs.StringVar(&f.app, "app", version, "version string shown in the interface")

	fs.Float64Var(&f.capital, "capital", 1000.00, "initial paper capital in USDT")
	fs.StringVar(&f.window, "window", "7d", "evaluation window preset: '7d' (last week), '30d' (last month), '14d' (2 weeks)")
	fs.Float64Var(&f.durationH, "duration-hours", 0, "target autonomous duration in hours (0 = infer from --window, e.g. 168h for 7d, 720h for 30d)")
	fs.Float64Var(&f.maxDD, "max-drawdown", 2.0, "maximum allowable drawdown percentage before circuit breaker halts")
	fs.StringVar(&f.symbols, "symbols", "BTCUSDT,ETHUSDT", "comma-separated trading symbols")
	fs.BoolVar(&f.statusOnly, "status", false, "print terminal summary of active dry run and exit")
	fs.BoolVar(&f.reportOnly, "report", false, "generate and print institutional EOD risk & tear-sheet report and exit")
	fs.BoolVar(&f.enableFallback, "fallback", false, "opt-in to local synthetic feed generation when offline (default false)")
	fs.BoolVar(&f.enableAllModels, "enable-all-models", false, "re-enable all registered models in ensemble regardless of ablation gate")
	fs.BoolVar(&f.extendOnShutdown, "extend-on-shutdown", true, "automatically extend target end time by offline duration upon shutdown/power-cycle recovery (default true)")
	fs.BoolVar(&f.flattenOnShutdown, "flatten-on-shutdown", true, "automatically liquidate all open positions to cash at current mark price on shutdown (default true)")
	fs.StringVar(&f.wsURL, "ws-url", "", "custom Binance public WebSocket URL (default auto)")
	return f
}

func runDryRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dry-run", flag.ContinueOnError)
	f := addDryRunFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dDir := f.dataDir
	if dDir == "" {
		dDir = defaultDataDir()
	}

	stateFile := filepath.Join(dDir, "paper", "dryrun_state.json")

	// If --report is requested, print Markdown EOD tear-sheet report and return
	if f.reportOnly {
		return printDailyReport(stateFile, dDir)
	}

	// If --status is requested, print terminal report and return
	if f.statusOnly {
		return printDryRunStatus(stateFile)
	}

	store, err := openStore(f.secretsDir)
	if err != nil {
		return err
	}
	instance, _ := discovery.InstanceID(secretsDirOf(f.secretsDir))

	logDir := f.logDir
	if logDir == "" {
		logDir = defaultLogDir()
	}
	if lf, err := openLog(logDir, "dryrun.log"); err == nil {
		defer lf.Close()
	}

	// Setup ring buffer & public Binance market stream ingestor
	ring := engine.NewSPSCRing(4096)

	symList := strings.Split(f.symbols, ",")
	for i := range symList {
		symList[i] = strings.TrimSpace(strings.ToUpper(symList[i]))
	}

	streamCfg := paper.DefaultStreamConfig()
	streamCfg.Symbols = symList
	streamCfg.EnableFallback = f.enableFallback
	if f.wsURL != "" {
		streamCfg.Endpoint = f.wsURL
	}
	ingestor := paper.NewMarketStreamIngestor(streamCfg, ring)
	ingestor.Start(ctx)
	defer ingestor.Stop()

	// Initialize dry run engine with selected window preset
	paperCfg := paper.DefaultDryRunConfig(dDir)
	paperCfg.InitialCapital = f.capital
	paperCfg.WindowPreset = f.window
	if f.durationH > 0 {
		paperCfg.TargetDuration = time.Duration(f.durationH) * time.Hour
	}
	paperCfg.MaxDrawdownPct = f.maxDD
	paperCfg.Symbols = symList
	paperCfg.EnableAllModels = f.enableAllModels
	paperCfg.ExtendOnShutdown = f.extendOnShutdown
	paperCfg.FlattenOnShutdown = f.flattenOnShutdown

	dryRunEngine, err := paper.NewDryRunEngine(paperCfg, ring, ingestor)
	if err != nil {
		return fmt.Errorf("failed to initialize dry run engine: %w", err)
	}
	dryRunEngine.Start(ctx)
	defer dryRunEngine.Stop()

	// Start Announcer and Browser for zero-config peering
	var browser *discovery.Browser
	if !f.noDiscover {
		b := &discovery.Browser{Token: storeToken(store, secrets.PeerToken), SelfID: instance}
		if err := b.Listen(); err == nil {
			browser = b
			defer b.Close()
		}
	}
	port := parsePort(f.addr)
	if a := startAnnouncer(discovery.RoleEngine, port, storeToken(store, secrets.PeerToken), instance, f.app, browser); a != nil {
		defer a.Stop()
	}

	// Initialize local experiment manager so research engine endpoints work co-located
	_ = os.MkdirAll(dDir, 0o755)
	_ = experiment.GenerateStandardDatasets(dDir)

	expCfg, err := config.Load("configs/production.yaml")
	if err != nil {
		expCfg, _ = config.Load("configs/dev.yaml")
	}
	if expCfg == nil {
		expCfg = &config.Config{
			Symbols: []config.Symbol{
				{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"},
				{Name: "ETH", VenueSym: "ETHUSDT", Venue: "binance"},
			},
			Targets: map[string][]string{
				"BTC": {"binance:BTCUSDT"},
				"ETH": {"binance:ETHUSDT"},
			},
		}
	}

	mgr, err := experiment.NewManager(dDir, expCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot initialize experiment manager: %v\n", err)
	}

	// Launch Dashboard Server exposing 2-week paper run, blotter and co-located engine
	srv, err := dashboard.New(dashboard.Options{
		Addr:        f.addr,
		Token:       storeToken(store, secrets.DashboardToken),
		PeerToken:   storeToken(store, secrets.PeerToken),
		Keys:        store,
		LogFiles:    logFiles(logDir),
		DataDirs:    dataDirs(f.dataDir),
		Role:        discovery.RoleEngine,
		App:         f.app,
		Discover:    browser,
		Instance:    instance,
		SessionTTL:  12 * time.Hour,
		ClusterMode: "pi5_only",
		Manager:     mgr,
		DryRun:      dryRunEngine,
	})
	if err != nil {
		return err
	}

	st := dryRunEngine.State()
	targetHours := int(st.TargetEndTime.Sub(st.StartTime).Hours())
	if targetHours <= 0 {
		targetHours = st.TargetDays * 24
	}
	fmt.Printf("\n=======================================================\n")
	fmt.Printf(" [PIQUANT] %d-DAY AUTONOMOUS PAPER DRY RUN INITIATED\n", st.TargetDays)
	fmt.Printf("=======================================================\n")
	fmt.Printf(" Window Preset:   %s (%d Days / %dh)\n", st.WindowPreset, st.TargetDays, targetHours)
	fmt.Printf(" Initial Capital: $%.2f USDT\n", st.InitialCapital)
	fmt.Printf(" Active Universe: %s\n", strings.Join(symList, ", "))
	fmt.Printf(" Max Drawdown Limit: %.2f%% (Circuit Breaker)\n", paperCfg.MaxDrawdownPct)
	fmt.Printf(" State Checkpoint: %s\n", paperCfg.StateFile)
	fmt.Printf(" Monitoring Dashboard: http://%s\n", listenHint(f.addr))
	fmt.Printf("=======================================================\n\n")

	return srv.ListenAndServe(ctx)
}

func printDryRunStatus(stateFile string) error {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No active 2-week dry run checkpoint found at %s.\n", stateFile)
			fmt.Println("Run 'piquant dry-run' to start the 14-day autonomous paper money run.")
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	var st paper.DryRunState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("corrupted state file %s: %w", stateFile, err)
	}

	now := time.Now()
	rem := st.TargetEndTime.Sub(now)
	if rem < 0 {
		rem = 0
	}

	ds := st.DataSource
	if ds == "" {
		ds = "live"
	}
	if st.QuotingPaused {
		ds += " (QUOTING PAUSED)"
	}

	totDays := st.TargetDays
	if totDays <= 0 {
		totDays = 14
	}
	presetName := st.WindowPreset
	if presetName == "" {
		presetName = fmt.Sprintf("%dd", totDays)
	}

	fmt.Println("=======================================================")
	fmt.Printf(" PIQUANT %d-DAY (%s) AUTONOMOUS DRY RUN STATUS REPORT\n", totDays, strings.ToUpper(presetName))
	fmt.Println("=======================================================")
	fmt.Printf(" Status:          %s\n", pick(!st.CircuitHalted, "ACTIVE (OK)", "CIRCUIT BREAKER HALTED"))
	fmt.Printf(" Window Preset:   %s (%d days)\n", presetName, totDays)
	fmt.Printf(" Data Source:     %s\n", ds)
	fmt.Printf(" Feed Down Time:  %.1fs\n", st.FeedDownSeconds)
	fmt.Printf(" Progress:        Day %d of %d (%.1f hours remaining)\n", st.DaysElapsed, totDays, rem.Hours())
	fmt.Printf(" Start Time:      %s\n", st.StartTime.Format(time.RFC3339))
	fmt.Printf(" Target End:      %s\n", st.TargetEndTime.Format(time.RFC3339))
	fmt.Printf(" Initial Capital: $%.2f USDT\n", st.InitialCapital)
	fmt.Printf(" Current Equity:  $%.2f USDT (%+.2f%%)\n", st.Equity, st.TotalReturnPct)
	fmt.Printf(" Cash:            $%.2f USDT\n", st.Cash)
	fmt.Printf(" Realized PnL:    $%.2f USDT\n", st.RealizedPnL)
	fmt.Printf(" Unrealized PnL:  $%.2f USDT\n", st.UnrealizedPnL)
	fmt.Printf(" Peak Equity:     $%.2f USDT\n", st.PeakEquity)
	fmt.Printf(" Max Drawdown:    %.2f%%\n", st.MaxDrawdownPct)
	fmt.Printf(" Total Trades:    %d (Win Rate: %.1f%%)\n", st.TotalTrades, st.WinRate)

	if st.ShutdownCount > 0 {
		fmt.Printf(" Shutdown Recovery: %d resumes (%.1fh compensated downtime)\n", st.ShutdownCount, st.OfflineDowntimeSeconds/3600.0)
		if !st.LastResumeTime.IsZero() {
			fmt.Printf(" Last Resumed:    %s\n", st.LastResumeTime.Format(time.RFC3339))
		}
	}

	if st.CircuitHalted {
		fmt.Printf(" Circuit Halt:    %s\n", st.CircuitHaltReason)
	}

	fmt.Println("-------------------------------------------------------")
	fmt.Println(" Open Inventory Positions:")
	for sym, pos := range st.Positions {
		fmt.Printf("   %s: Size=%.4f Entry=$%.2f Mark=$%.2f UnrealizedPnL=$%.2f Trades=%d\n",
			sym, pos.Size, pos.AvgEntryPrice, pos.MarkPrice, pos.UnrealizedPnL, pos.TotalTrades)
	}
	fmt.Println("=======================================================")
	return nil
}

func printDailyReport(stateFile, dDir string) error {
	var st paper.DryRunState
	if data, err := os.ReadFile(stateFile); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &st)
	} else {
		// Provide default initialized institutional baseline if no state file exists yet
		st = paper.DryRunState{
			StartTime:      time.Now().Add(-24 * time.Hour),
			TargetEndTime:  time.Now().Add(13 * 24 * time.Hour),
			InitialCapital: 100000.00,
			Cash:           100000.00,
			Equity:         100000.00,
			PeakEquity:     100000.00,
			DaysElapsed:    1,
			HoursRemaining: 312.0,
			DataSource:     "live",
		}
	}

	gen := paper.NewDailyReportGenerator()
	now := time.Now()
	report := gen.GenerateReport(st, now)

	// Save to reports dir if possible
	reportsDir := filepath.Join(dDir, "paper", "reports")
	_ = gen.SaveReport(st, reportsDir, now)

	fmt.Print(report)
	return nil
}
