package paper

import (
	"context"
	"os"
	"testing"
	"time"

	"piquant-live/internal/engine"
)

func TestLiveBinanceStreamIngestion60s(t *testing.T) {
	if testing.Short() || os.Getenv("TEST_LIVE_STREAM") != "1" {
		t.Skip("skipping 60s live stream test (set TEST_LIVE_STREAM=1 to run)")
	}

	ring := engine.NewSPSCRing(8192)
	cfg := DefaultStreamConfig()
	cfg.EnableFallback = false // Strict live data only!

	t.Logf("Initiating 60-second live Binance ingestion test with endpoint: %s", cfg.Endpoint)
	ingestor := NewMarketStreamIngestor(cfg, ring)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ingestor.Start(ctx)
	defer ingestor.Stop()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			total, depths, trades, gaps, recons, fallbacks, lastNs := ingestor.Stats()
			btcBook, hasBTC := ingestor.GetL2Book("BTCUSDT")
			ethBook, hasETH := ingestor.GetL2Book("ETHUSDT")

			btcMid, _ := btcBook.MidPrice()
			ethMid, _ := ethBook.MidPrice()

			t.Logf("=== 60-SECOND LIVE INGESTION REPORT ===")
			t.Logf("Duration: %v", time.Since(start))
			t.Logf("Total Events: %d", total)
			t.Logf("Depth Updates: %d", depths)
			t.Logf("Trades: %d", trades)
			t.Logf("Sequence Gaps: %d", gaps)
			t.Logf("Reconnects: %d", recons)
			t.Logf("Fallbacks: %d", fallbacks)
			t.Logf("Last Event Timestamp: %s", time.Unix(0, lastNs).Format(time.RFC3339Nano))
			t.Logf("BTCUSDT: hasBook=%v BestBid=%.2f BestAsk=%.2f Mid=%.2f",
				hasBTC, btcBook.Bids[0].Price, btcBook.Asks[0].Price, btcMid)
			t.Logf("ETHUSDT: hasBook=%v BestBid=%.2f BestAsk=%.2f Mid=%.2f",
				hasETH, ethBook.Bids[0].Price, ethBook.Asks[0].Price, ethMid)
			t.Logf("=======================================")

			if total == 0 || depths == 0 || trades == 0 {
				t.Fatalf("FAILED: No live data received (total=%d, depths=%d, trades=%d)", total, depths, trades)
			}
			if btcMid <= 10000 || ethMid <= 500 {
				t.Fatalf("FAILED: Prices look synthetic or invalid (BTC=%.2f, ETH=%.2f)", btcMid, ethMid)
			}
			return
		case <-ticker.C:
			tot, dep, tr, _, _, _, _ := ingestor.Stats()
			t.Logf("[%v elapsed] Received %d events (depths=%d, trades=%d)", time.Since(start).Round(time.Second), tot, dep, tr)
		}
	}
}
