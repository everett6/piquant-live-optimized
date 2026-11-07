package paper

import (
	"context"
	"os"
	"testing"
	"time"

	"piquant-live/internal/engine"
)

func TestLiveDryRunExecutesTrades(t *testing.T) {
	if os.Getenv("TEST_LIVE_DRYRUN") != "1" {
		t.Skip("skipping live dry run execution test (set TEST_LIVE_DRYRUN=1 to run)")
	}

	tempDir := t.TempDir()
	paperCfg := DefaultDryRunConfig(tempDir)
	paperCfg.OrderLatency = 50 * time.Millisecond

	ring := engine.NewSPSCRing(8192)
	streamCfg := DefaultStreamConfig()
	streamCfg.EnableFallback = false // Strict live Binance feed only

	ingestor := NewMarketStreamIngestor(streamCfg, ring)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	ingestor.Start(ctx)
	defer ingestor.Stop()

	dryRun, err := NewDryRunEngine(paperCfg, ring, ingestor)
	if err != nil {
		t.Fatalf("failed to initialize dry run: %v", err)
	}

	dryRun.Start(ctx)
	defer dryRun.Stop()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			st := dryRun.State()
			fills := dryRun.RecentFills()

			t.Logf("=== LIVE DRY RUN 75s TRADING RESULTS ===")
			t.Logf("Data Source: %s", st.DataSource)
			t.Logf("Total Trades: %d (Maker: %d, Taker: %d)", st.TotalTrades, st.Positions["BTCUSDT"].MakerTrades+st.Positions["ETHUSDT"].MakerTrades, st.Positions["BTCUSDT"].TakerTrades+st.Positions["ETHUSDT"].TakerTrades)
			t.Logf("Realized PnL: $%.2f USDT", st.RealizedPnL)
			t.Logf("Unrealized PnL: $%.2f USDT", st.UnrealizedPnL)
			t.Logf("Total Equity: $%.2f USDT", st.Equity)
			t.Logf("Win Rate: %.1f%%", st.WinRate)
			t.Logf("Recent Fills Count: %d", len(fills))
			for i, f := range fills {
				t.Logf("  Fill %d: %s %s %s size=%.4f price=$%.2f edge=%.2fbps pnl=$%.4f source=%s",
					i+1, f.Symbol, f.Role, f.Side, f.Size, f.Price, f.EdgeBps, f.PnLRealized, f.DataSource)
			}
			t.Logf("========================================")

			if st.TotalTrades == 0 {
				t.Fatalf("Expected trades to occur on real Binance data, but got 0 trades")
			}
			return
		case <-ticker.C:
			st := dryRun.State()
			t.Logf("[%v elapsed] TotalTrades=%d RealizedPnL=%.2f Equity=%.2f",
				time.Since(start).Round(time.Second), st.TotalTrades, st.RealizedPnL, st.Equity)
		}
	}
}
