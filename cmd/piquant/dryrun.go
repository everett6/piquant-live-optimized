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

	"piquant-live/internal/dashboard"
	"piquant-live/internal/discovery"
	"piquant-live/internal/engine"
	"piquant-live/internal/paper"
	"piquant-live/internal/secrets"
)

type dryRunFlags struct {
	serveFlags
	capital    float64
	durationH  float64
	maxDD      float64
	symbols    string
	statusOnly bool
}

func addDryRunFlags(fs *flag.FlagSet) *dryRunFlags {
	f := &dryRunFlags{}
	fs.StringVar(&f.addr, "addr", ":8080", "listen address for the monitoring dashboard, e.g. :8080")
	fs.StringVar(&f.secretsDir, "secrets-dir", "", "secrets directory (default ~/.config/piquant)")
	fs.StringVar(&f.dataDir, "data", "", "data directory where paper checkpoints live")
	fs.StringVar(&f.logDir, "log-dir", "", "directory holding the service logs")
	fs.BoolVar(&f.noDiscover, "no-discovery", false, "do not announce or search for the peer board")
	fs.StringVar(&f.app, "app", version, "version string shown in the interface")

	fs.Float64Var(&f.capital, "capital", 100000.00, "initial paper capital in USDT")
	fs.Float64Var(&f.durationH, "duration-hours", 336.0, "target autonomous duration in hours (default 336h = 14 days)")
	fs.Float64Var(&f.maxDD, "max-drawdown", 2.0, "maximum allowable drawdown percentage before circuit breaker halts")
	fs.StringVar(&f.symbols, "symbols", "BTCUSDT,ETHUSDT", "comma-separated trading symbols")
	fs.BoolVar(&f.statusOnly, "status", false, "print terminal summary of active dry run and exit")
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
	ingestor := paper.NewMarketStreamIngestor(streamCfg, ring)
	ingestor.Start(ctx)
	defer ingestor.Stop()

	// Initialize 14-day dry run engine
	paperCfg := paper.DefaultDryRunConfig(dDir)
	paperCfg.InitialCapital = f.capital
	paperCfg.TargetDuration = time.Duration(f.durationH) * time.Hour
	paperCfg.MaxDrawdownPct = f.maxDD
	paperCfg.Symbols = symList

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
	if a := startAnnouncer(discovery.RoleEngine, port, storeToken(store, secrets.PeerToken), instance, f.app); a != nil {
		defer a.Stop()
	}

	// Launch Dashboard Server exposing 2-week paper run and blotter
	srv, err := dashboard.New(dashboard.Options{
		Addr:       f.addr,
		Token:      storeToken(store, secrets.DashboardToken),
		PeerToken:  storeToken(store, secrets.PeerToken),
		Keys:       store,
		LogFiles:   logFiles(logDir),
		DataDirs:   dataDirs(f.dataDir),
		Role:       discovery.RoleEngine,
		App:        f.app,
		Discover:   browser,
		Instance:   instance,
		SessionTTL: 12 * time.Hour,
		DryRun:     dryRunEngine,
	})
	if err != nil {
		return err
	}

	st := dryRunEngine.State()
	fmt.Printf("\n=======================================================\n")
	fmt.Printf(" [PIQUANT] 14-DAY AUTONOMOUS PAPER DRY RUN INITIATED\n")
	fmt.Printf("=======================================================\n")
	fmt.Printf(" Target Duration: %v (336 Hours)\n", paperCfg.TargetDuration)
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

	fmt.Println("=======================================================")
	fmt.Printf(" PIQUANT 2-WEEK AUTONOMOUS DRY RUN STATUS REPORT\n")
	fmt.Println("=======================================================")
	fmt.Printf(" Status:          %s\n", pick(!st.CircuitHalted, "ACTIVE (OK)", "CIRCUIT BREAKER HALTED"))
	fmt.Printf(" Progress:        Day %d of 14 (%.1f hours remaining)\n", st.DaysElapsed, rem.Hours())
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
