package paper

import (
	"testing"
	"time"
)

func TestDryRunWatchdogStates(t *testing.T) {
	wd := NewDryRunWatchdog()
	nowNs := int64(1_700_000_000_000_000_000)

	// 1. Nominal operating conditions: fresh feed (20ms old), 10% ring load, 55C temp, no throttle
	repNom := wd.Check(nowNs, nowNs-20_000_000, 100, 1024, 55.0, false)
	if repNom.State != WatchdogNominal {
		t.Errorf("expected WatchdogNominal, got %s", repNom.State)
	}
	if repNom.ThermalThrottle || repNom.Backpressure || repNom.StaleFeed {
		t.Errorf("expected all fault flags false in nominal state, got %+v", repNom)
	}

	// 2. SPSC Ring Backpressure (>80% occupancy): should degrade to DEGRADED_PASSIVE_ONLY
	repBack := wd.Check(nowNs, nowNs-30_000_000, 900, 1024, 58.0, false)
	if repBack.State != WatchdogDegradedPassiveOnly || !repBack.Backpressure {
		t.Errorf("expected DEGRADED_PASSIVE_ONLY on 87.9%% ring load, got %+v", repBack)
	}

	// 3. Raspberry Pi 5 Thermal Throttling (>= 78.0C or hardware throttled flag): should degrade to DEGRADED_PASSIVE_ONLY
	repTherm := wd.Check(nowNs, nowNs-30_000_000, 128, 1024, 79.5, true)
	if repTherm.State != WatchdogDegradedPassiveOnly || !repTherm.ThermalThrottle {
		t.Errorf("expected DEGRADED_PASSIVE_ONLY on 79.5C thermal throttle, got %+v", repTherm)
	}

	// 4. Stale Market Feed (>2.5s silence): should trigger HALTED_STALE_FEED
	repStale := wd.Check(nowNs, nowNs-4_000_000_000, 64, 1024, 55.0, false)
	if repStale.State != WatchdogHaltedStaleFeed || !repStale.StaleFeed {
		t.Errorf("expected HALTED_STALE_FEED on 4000ms feed silence, got %+v", repStale)
	}
	if repStale.StaleMs < 3999.0 {
		t.Errorf("expected StaleMs ~4000ms, got %.2f", repStale.StaleMs)
	}
}

func TestComputeDryRunTearSheet(t *testing.T) {
	now := time.Now()
	st := DryRunState{
		StartTime:      now.Add(-48 * time.Hour),
		TargetEndTime:  now.Add(12 * 24 * time.Hour),
		LastCheckpoint: now,
		InitialCapital: 100000.0,
		Cash:           99200.0,
		Equity:         100650.0,
		PeakEquity:     100700.0,
		RealizedPnL:    520.0,
		UnrealizedPnL:  130.0,
		TotalReturnPct: 0.65,
		MaxDrawdownPct: 0.14,
		TotalTrades:    60,
		WinningTrades:  39,
		LosingTrades:   21,
		WinRate:        65.0,
		SharpeRatio:    2.80,
		DaysElapsed:    2,
		HoursRemaining: 288.0,
		DailyReturns:   []float64{0.28, 0.37, -0.06, 0.22},
		Positions: map[string]*Position{
			"BTCUSDT": {
				Symbol:          "BTCUSDT",
				Size:            0.10,
				MakerTrades:     34,
				TakerTrades:     6,
				TotalTrades:     40,
				TotalVolumeUSDT: 210000.0,
			},
			"ETHUSDT": {
				Symbol:          "ETHUSDT",
				Size:            0.50,
				MakerTrades:     17,
				TakerTrades:     3,
				TotalTrades:     20,
				TotalVolumeUSDT: 68000.0,
			},
		},
	}

	ts := ComputeDryRunTearSheet(st)
	if ts.CurrentEquity != 100650.0 {
		t.Errorf("expected CurrentEquity 100650.0, got %.2f", ts.CurrentEquity)
	}
	if ts.SortinoRatio <= 0 {
		t.Errorf("expected positive SortinoRatio, got %.3f", ts.SortinoRatio)
	}
	if ts.CalmarRatio <= 0 {
		t.Errorf("expected positive CalmarRatio, got %.3f", ts.CalmarRatio)
	}
	if ts.ProfitFactor <= 1.0 {
		t.Errorf("expected ProfitFactor > 1.0 for winning book, got %.3f", ts.ProfitFactor)
	}
	expectedMakerRatio := 51.0 / 60.0
	if ts.MakerFillRatio < expectedMakerRatio-0.01 || ts.MakerFillRatio > expectedMakerRatio+0.01 {
		t.Errorf("expected MakerFillRatio ~%.3f, got %.3f", expectedMakerRatio, ts.MakerFillRatio)
	}
	if ts.AnnualizedReturnPct <= 0 {
		t.Errorf("expected positive AnnualizedReturnPct, got %.2f%%", ts.AnnualizedReturnPct)
	}
	if ts.TotalVolumeUSDT != 278000.0 {
		t.Errorf("expected TotalVolumeUSDT 278000.0, got %.2f", ts.TotalVolumeUSDT)
	}
}

func BenchmarkDryRunWatchdogCheck(b *testing.B) {
	wd := NewDryRunWatchdog()
	nowNs := int64(1_700_000_000_000_000_000)
	lastNs := nowNs - 25_000_000
	b.ReportAllocs()
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		rep := wd.Check(nowNs, lastNs, 128, 1024, 56.4, false)
		sink += rep.StaleMs + rep.RingLoadPct
	}
	if sink < 0 {
		b.Fatal("invalid sink")
	}
}
