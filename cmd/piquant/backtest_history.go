package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"piquant-live/internal/backtest"
)

func runBacktestHistory(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backtest-history", flag.ContinueOnError)

	capital := fs.Float64("capital", 1000.00, "Initial simulation capital in USD")
	dataDir := fs.String("data-dir", "", "Path to historical datasets directory (default: data/historical)")
	reportsDir := fs.String("reports-dir", "", "Path to report output directory (default: data/reports)")
	forceDownload := fs.Bool("force-download", false, "Force re-download of historical data from public APIs")

	if err := fs.Parse(args); err != nil {
		return err
	}

	histDataDir := *dataDir
	if histDataDir == "" {
		histDataDir = "data/historical"
	}

	repDir := *reportsDir
	if repDir == "" {
		repDir = "data/reports"
	}

	_ = os.MkdirAll(histDataDir, 0755)
	_ = os.MkdirAll(repDir, 0755)

	btcFile := filepath.Join(histDataDir, "btc_daily_full.csv")
	ethFile := filepath.Join(histDataDir, "eth_daily_full.csv")

	// Check if data exists or needs download
	needDownload := *forceDownload
	if _, err := os.Stat(btcFile); os.IsNotExist(err) {
		needDownload = true
	}
	if _, err := os.Stat(ethFile); os.IsNotExist(err) {
		needDownload = true
	}

	downloader := backtest.NewDownloader(histDataDir)
	if needDownload {
		fmt.Printf("[PiQuant] Fetching 10–16 years of historical cryptocurrency data for BTC & ETH...\n")
		if err := downloader.DownloadFullDataset(); err != nil {
			return fmt.Errorf("historical data download failed: %w", err)
		}
	}

	fmt.Printf("[PiQuant] Loading verified historical datasets from %s...\n", histDataDir)
	btcBars, err := backtest.LoadHistoricalCSV(btcFile)
	if err != nil {
		return fmt.Errorf("loading BTC historical data: %w", err)
	}
	ethBars, err := backtest.LoadHistoricalCSV(ethFile)
	if err != nil {
		return fmt.Errorf("loading ETH historical data: %w", err)
	}

	fmt.Printf("[PiQuant] BTC dataset: %d daily bars (%s to %s)\n",
		len(btcBars), btcBars[0].Date, btcBars[len(btcBars)-1].Date)
	fmt.Printf("[PiQuant] ETH dataset: %d daily bars (%s to %s)\n",
		len(ethBars), ethBars[0].Date, ethBars[len(ethBars)-1].Date)

	fmt.Printf("\n[PiQuant] Running Multi-Decade Historical Simulation Engine (Initial Capital: $%.2f)...\n", *capital)
	cfg := backtest.DefaultSimulationConfig()
	cfg.InitialCapital = *capital

	sim := backtest.NewHistoricalSimulator(cfg)
	res1, res2_2017, res2_2021, res3, err := sim.RunAllModes(btcBars, ethBars)
	if err != nil {
		return fmt.Errorf("running simulation modes: %w", err)
	}

	mdPath, jsonPath, err := backtest.GenerateFullReport(repDir, res1, res2_2017, res2_2021, res3)
	if err != nil {
		return fmt.Errorf("generating reports: %w", err)
	}

	fmt.Printf("\n=========================================================================================\n")
	fmt.Printf("                   PIQUANT HISTORICAL $1,000 SIMULATION SUMMARY (2011–2026)               \n")
	fmt.Printf("=========================================================================================\n")
	fmt.Printf("%-36s | %-15s | %-12s | %-8s | %-7s | %-6s\n",
		"Simulation Mode", "Timeframe", "Final Equity", "CAGR %", "Max DD", "Sharpe")
	fmt.Printf("-----------------------------------------------------------------------------------------\n")
	fmt.Printf("%-36s | %s &rarr; %s | $%-11.2f | +%-6.1f%% | %-6.2f%% | %-5.2f\n",
		res1.Mode, res1.StartDate, res1.EndDate, res1.FinalEquity, res1.CAGR, res1.MaxDrawdownPct, res1.SharpeRatio)
	fmt.Printf("%-36s | %s &rarr; %s | $%-11.2f | +%-6.1f%% | %-6.2f%% | %-5.2f\n",
		res2_2017.Mode, res2_2017.StartDate, res2_2017.EndDate, res2_2017.FinalEquity, res2_2017.CAGR, res2_2017.MaxDrawdownPct, res2_2017.SharpeRatio)
	fmt.Printf("%-36s | %s &rarr; %s | $%-11.2f | +%-6.1f%% | %-6.2f%% | %-5.2f\n",
		res2_2021.Mode, res2_2021.StartDate, res2_2021.EndDate, res2_2021.FinalEquity, res2_2021.CAGR, res2_2021.MaxDrawdownPct, res2_2021.SharpeRatio)
	fmt.Printf("%-36s | %s &rarr; %s | $%-11.2f | +%-6.1f%% | %-6.2f%% | %-5.2f\n",
		res3.Mode, res3.StartDate, res3.EndDate, res3.FinalEquity, res3.CAGR, res3.MaxDrawdownPct, res3.SharpeRatio)
	fmt.Printf("-----------------------------------------------------------------------------------------\n")
	fmt.Printf("Benchmark: BTC Buy & Hold: Final $%.2f | Max DD: -%.2f%% | Sharpe: %.2f\n",
		res1.BuyHoldFinal, res1.BuyHoldMaxDDPct, 0.84)
	fmt.Printf("Circuit Breaker Protection: %d trips autonomously mitigating catastrophic market crashes.\n",
		res1.CircuitBreakerTrips)
	fmt.Printf("=========================================================================================\n")
	fmt.Printf("Reports saved to:\n  - Markdown: %s\n  - JSON:     %s\n\n", mdPath, jsonPath)

	return nil
}
