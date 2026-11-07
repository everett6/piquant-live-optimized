package paper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyReportGenerator_GenerateReport(t *testing.T) {
	gen := NewDailyReportGenerator()
	gen.TempOverride = 52.4
	gen.ThrottledOverride = false

	state := DryRunState{
		StartTime:       time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		TargetEndTime:   time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
		InitialCapital:  100000.0,
		Cash:            95000.0,
		Equity:          102450.80,
		PeakEquity:      102680.00,
		RealizedPnL:     2150.80,
		UnrealizedPnL:   300.00,
		TotalReturnPct:  2.45,
		MaxDrawdownPct:  0.42,
		CircuitHalted:   false,
		TotalTrades:     1428,
		WinningTrades:   892,
		LosingTrades:    536,
		WinRate:         62.4,
		DaysElapsed:     5,
		HoursRemaining:  216.0,
		FeedDownSeconds: 0.0,
		Positions: map[string]*Position{
			"BTCUSDT": {
				Symbol:          "BTCUSDT",
				Size:            0.25,
				AvgEntryPrice:   67200.0,
				MarkPrice:       67300.0,
				UnrealizedPnL:   25.0,
				MakerTrades:     1178,
				TakerTrades:     250,
				TotalTrades:     1428,
				TotalVolumeUSDT: 2500000.0,
			},
		},
	}

	reportDate := time.Date(2026, 11, 5, 23, 59, 59, 0, time.UTC)
	report := gen.GenerateReport(state, reportDate)

	requiredSections := []string{
		"# PiQuant Institutional Daily Risk & Performance Tear-Sheet",
		"**Report Date:** 2026-11-05",
		"Starting Capital",
		"$100,000.00 USDT",
		"Closing Capital (Equity)",
		"$102,450.80 USDT",
		"Daily Net PnL",
		"+$2,450.80 USDT",
		"Return %",
		"+2.45%",
		"Peak Equity",
		"$102,680.00 USDT",
		"Max Drawdown %",
		"0.42%",
		"Total Executed Trades",
		"1428",
		"Win Rate %",
		"62.4%",
		"Maker / Taker Ratio",
		"Conformal Gate Pass Rate",
		"94.8%",
		"Circuit Breaker Trips",
		"0 Trips (Normal Operating Bounds)",
		"Feed Down Disconnect",
		"Peak CPU Temperature",
		"52.4 °C",
		"Active Throttling Flags",
		"None (All thermal & voltage rails nominal)",
	}

	for _, sec := range requiredSections {
		if !strings.Contains(report, sec) {
			t.Errorf("missing required section/metric in report: %q", sec)
		}
	}
}

func TestDailyReportGenerator_SaveReport(t *testing.T) {
	tmpDir := t.TempDir()
	gen := NewDailyReportGenerator()

	state := DryRunState{
		InitialCapital: 100000.0,
		Equity:         100500.0,
		RealizedPnL:    500.0,
		DaysElapsed:    1,
	}

	date := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	err := gen.SaveReport(state, tmpDir, date)
	if err != nil {
		t.Fatalf("SaveReport failed: %v", err)
	}

	expectedFile := filepath.Join(tmpDir, "report_2026-11-06.md")
	data, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("failed to read expected saved report %s: %v", expectedFile, err)
	}

	if len(data) == 0 {
		t.Fatalf("saved report file is empty")
	}

	if !strings.Contains(string(data), "# PiQuant Institutional Daily Risk") {
		t.Errorf("saved report content did not contain expected title")
	}
}

func BenchmarkDailyReportGenerator_GenerateReport(b *testing.B) {
	gen := NewDailyReportGenerator()
	gen.TempOverride = 48.5
	state := DryRunState{
		InitialCapital: 100000.0,
		Equity:         101200.0,
		RealizedPnL:    1200.0,
		TotalTrades:    350,
		WinningTrades:  220,
		DaysElapsed:    2,
	}
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gen.GenerateReport(state, now)
	}
}
