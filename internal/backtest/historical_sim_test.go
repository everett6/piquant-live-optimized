package backtest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalSimulatorAllModes(t *testing.T) {
	dataDir := filepath.Join("..", "..", "data", "historical")
	reportsDir := filepath.Join("..", "..", "data", "reports")

	btcPath := filepath.Join(dataDir, "btc_daily_full.csv")
	ethPath := filepath.Join(dataDir, "eth_daily_full.csv")

	btcBars, err := LoadHistoricalCSV(btcPath)
	if err != nil || len(btcBars) == 0 {
		t.Fatalf("Failed to load BTC historical bars: %v", err)
	}

	ethBars, err := LoadHistoricalCSV(ethPath)
	if err != nil || len(ethBars) == 0 {
		t.Fatalf("Failed to load ETH historical bars: %v", err)
	}

	cfg := DefaultSimulationConfig()
	cfg.InitialCapital = 1000.00

	sim := NewHistoricalSimulator(cfg)
	res1, res2_2017, res2_2021, res3, err := sim.RunAllModes(btcBars, ethBars)
	if err != nil {
		t.Fatalf("RunAllModes failed: %v", err)
	}

	// 1. Verify Mode 1
	if res1.InitialCapital != 1000.00 {
		t.Errorf("Mode 1 initial capital expected 1000, got %.2f", res1.InitialCapital)
	}
	if res1.FinalEquity <= res1.InitialCapital {
		t.Errorf("Mode 1 final equity (%.2f) should exceed initial capital", res1.FinalEquity)
	}
	if res1.SharpeRatio < 1.0 {
		t.Errorf("Mode 1 Sharpe ratio expected > 1.0, got %.2f", res1.SharpeRatio)
	}
	if res1.MaxDrawdownPct > 25.0 {
		t.Errorf("Mode 1 Max DD (%.2f%%) exceeded 25%% safety limit", res1.MaxDrawdownPct)
	}
	if len(res1.AnnualBreakdown) < 15 {
		t.Errorf("Mode 1 expected at least 15 annual metrics, got %d", len(res1.AnnualBreakdown))
	}

	t.Logf("=== Mode 1: Full 15-Year Compounded ===")
	t.Logf("Timeframe: %s to %s", res1.StartDate, res1.EndDate)
	t.Logf("Start Capital: $%.2f | Final Equity: $%.2f | Total Return: +%.1f%% | CAGR: +%.1f%%",
		res1.InitialCapital, res1.FinalEquity, res1.TotalReturnPct, res1.CAGR)
	t.Logf("Sharpe: %.2f | Sortino: %.2f | Max DD: %.2f%% | Total Trades: %d | Circuit Trips: %d",
		res1.SharpeRatio, res1.SortinoRatio, res1.MaxDrawdownPct, res1.TotalTrades, res1.CircuitBreakerTrips)
	t.Logf("BTC B&H Final: $%.2f | BTC B&H Max DD: -%.2f%%",
		res1.BuyHoldFinal, res1.BuyHoldMaxDDPct)

	// 2. Verify Mode 2A (Modern Binance Era 2017–2026)
	if res2_2017.InitialCapital != 1000.00 {
		t.Errorf("Mode 2A initial capital expected 1000, got %.2f", res2_2017.InitialCapital)
	}
	if res2_2017.FinalEquity <= res2_2017.InitialCapital {
		t.Errorf("Mode 2A final equity (%.2f) should exceed initial capital", res2_2017.FinalEquity)
	}
	if res2_2017.MaxDrawdownPct > 20.0 {
		t.Errorf("Mode 2A Max DD (%.2f%%) exceeded 20%% safety limit", res2_2017.MaxDrawdownPct)
	}
	t.Logf("=== Mode 2A: Modern Binance Era (2017–2026) ===")
	t.Logf("Timeframe: %s to %s", res2_2017.StartDate, res2_2017.EndDate)
	t.Logf("Start Capital: $%.2f | Final Equity: $%.2f | Total Return: +%.1f%% | CAGR: +%.1f%%",
		res2_2017.InitialCapital, res2_2017.FinalEquity, res2_2017.TotalReturnPct, res2_2017.CAGR)
	t.Logf("Sharpe: %.2f | Sortino: %.2f | Max DD: %.2f%% | Total Trades: %d",
		res2_2017.SharpeRatio, res2_2017.SortinoRatio, res2_2017.MaxDrawdownPct, res2_2017.TotalTrades)

	// 3. Verify Mode 2B (Recent 5-Year Era 2021–2026)
	if res2_2021.InitialCapital != 1000.00 {
		t.Errorf("Mode 2B initial capital expected 1000, got %.2f", res2_2021.InitialCapital)
	}
	if res2_2021.FinalEquity <= res2_2021.InitialCapital {
		t.Errorf("Mode 2B final equity (%.2f) should exceed initial capital", res2_2021.FinalEquity)
	}
	t.Logf("=== Mode 2B: Recent 5-Year Era (2021–2026) ===")
	t.Logf("Timeframe: %s to %s", res2_2021.StartDate, res2_2021.EndDate)
	t.Logf("Start Capital: $%.2f | Final Equity: $%.2f | Total Return: +%.1f%% | CAGR: +%.1f%%",
		res2_2021.InitialCapital, res2_2021.FinalEquity, res2_2021.TotalReturnPct, res2_2021.CAGR)
	t.Logf("Sharpe: %.2f | Sortino: %.2f | Max DD: %.2f%%",
		res2_2021.SharpeRatio, res2_2021.SortinoRatio, res2_2021.MaxDrawdownPct)

	// 4. Verify Mode 3 (Fixed-Notional Income Withdrawal)
	if res3.InitialCapital != 1000.00 {
		t.Errorf("Mode 3 initial capital expected 1000, got %.2f", res3.InitialCapital)
	}
	if res3.TotalNetProfit <= 0 {
		t.Errorf("Mode 3 total profit (%.2f) should be positive", res3.TotalNetProfit)
	}
	t.Logf("=== Mode 3: Fixed-Notional Income Withdrawal ===")
	t.Logf("Start Capital: $%.2f | Total Withdrawn Cash: $%.2f | Annual APR Avg: %.1f%%",
		res3.InitialCapital, res3.TotalNetProfit, res3.CAGR)

	// 5. Generate and verify report files
	mdPath, jsonPath, err := GenerateFullReport(reportsDir, res1, res2_2017, res2_2021, res3)
	if err != nil {
		t.Fatalf("GenerateFullReport failed: %v", err)
	}

	mdStat, err := os.Stat(mdPath)
	if err != nil || mdStat.Size() == 0 {
		t.Errorf("Report markdown file missing or empty: %s", mdPath)
	}
	jsonStat, err := os.Stat(jsonPath)
	if err != nil || jsonStat.Size() == 0 {
		t.Errorf("Report JSON file missing or empty: %s", jsonPath)
	}
	t.Logf("Reports generated successfully: %s (%d bytes), %s (%d bytes)",
		mdPath, mdStat.Size(), jsonPath, jsonStat.Size())
}
