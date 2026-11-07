package paper

import (
	"context"
	"testing"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/marketdata"
)

var (
	bybitSampleSnapshot = []byte(`{"topic":"orderbook.50.BTCUSDT","type":"snapshot","ts":1672304484978,"data":{"s":"BTCUSDT","b":[["65000.00","1.500"],["64999.50","2.100"],["64999.00","3.400"]],"a":[["65000.50","0.850"],["65001.00","1.200"],["65001.50","2.500"]],"u":12345678,"seq":123456789},"cts":1672304484976}`)
	bybitSampleDelta    = []byte(`{"topic":"orderbook.50.BTCUSDT","type":"delta","ts":1672304485000,"data":{"s":"BTCUSDT","b":[["65000.00","0.500"],["64999.00","0"]],"a":[["65000.50","0"],["65002.00","1.800"]],"u":12345679,"seq":123456790},"cts":1672304484998}`)
	bybitSampleTrades   = []byte(`{"topic":"publicTrade.BTCUSDT","type":"snapshot","ts":1672304484978,"data":[{"T":1672304484800,"s":"BTCUSDT","S":"Buy","v":"0.05","p":"65000.50","L":"PlusTick","i":"20f34a","BT":false},{"T":1672304484850,"s":"BTCUSDT","S":"Sell","v":"0.12","p":"65000.00","L":"MinusTick","i":"20f34b","BT":false}]}`)
)

func TestBybitFastParseOrderbook(t *testing.T) {
	var delta marketdata.OrderBookDelta
	var isSnapshot bool

	// Snapshot
	ok := FastParseBybitOrderbook(bybitSampleSnapshot, &delta, &isSnapshot)
	if !ok {
		t.Fatalf("expected snapshot parsing to succeed")
	}
	if !isSnapshot {
		t.Fatalf("expected isSnapshot to be true")
	}
	if delta.FinalUpdateID() != 12345678 {
		t.Fatalf("expected update ID 12345678, got %d", delta.FinalUpdateID())
	}
	if delta.NumBids != 3 || delta.NumAsks != 3 {
		t.Fatalf("expected 3 bids and 3 asks, got %d bids and %d asks", delta.NumBids, delta.NumAsks)
	}
	if delta.Bids[0].Price != 65000.00 || delta.Bids[0].Size != 1.500 {
		t.Fatalf("unexpected top bid: %v", delta.Bids[0])
	}
	if delta.Asks[0].Price != 65000.50 || delta.Asks[0].Size != 0.850 {
		t.Fatalf("unexpected top ask: %v", delta.Asks[0])
	}

	// Delta
	ok = FastParseBybitOrderbook(bybitSampleDelta, &delta, &isSnapshot)
	if !ok {
		t.Fatalf("expected delta parsing to succeed")
	}
	if isSnapshot {
		t.Fatalf("expected isSnapshot to be false")
	}
	if delta.FinalUpdateID() != 12345679 {
		t.Fatalf("expected update ID 12345679, got %d", delta.FinalUpdateID())
	}
}

func TestBybitFastParseTrades(t *testing.T) {
	tradesCount := 0
	FastParseBybitTrades(bybitSampleTrades, func(price, qty float64, side uint8, tradeTime int64) {
		tradesCount++
		if tradesCount == 1 {
			if price != 65000.50 || qty != 0.05 || side != 1 || tradeTime != 1672304484800 {
				t.Fatalf("unexpected trade 1: p=%f, q=%f, side=%d, time=%d", price, qty, side, tradeTime)
			}
		} else if tradesCount == 2 {
			if price != 65000.00 || qty != 0.12 || side != 2 || tradeTime != 1672304484850 {
				t.Fatalf("unexpected trade 2: p=%f, q=%f, side=%d, time=%d", price, qty, side, tradeTime)
			}
		}
	})

	if tradesCount != 2 {
		t.Fatalf("expected 2 trades, got %d", tradesCount)
	}
}

func TestBybitFastExtractSymbol(t *testing.T) {
	sym1 := FastExtractBybitSymbol(bybitSampleSnapshot)
	if sym1 != "BTCUSDT" {
		t.Fatalf("expected BTCUSDT, got %s", sym1)
	}

	ethTrade := []byte(`{"topic":"publicTrade.ETHUSDT","data":[{"s":"ETHUSDT","p":"3500.0"}]}`)
	sym2 := FastExtractBybitSymbol(ethTrade)
	if sym2 != "ETHUSDT" {
		t.Fatalf("expected ETHUSDT, got %s", sym2)
	}
}

func TestBybitStreamIngestorPushToRing(t *testing.T) {
	ring := engine.NewSPSCRing(1024)
	cfg := DefaultBybitStreamConfig()
	ingestor := NewBybitStreamIngestor(cfg, ring)

	// Feed snapshot payload
	ingestor.HandleStreamPayload(bybitSampleSnapshot)

	// Verify event landed in SPSC Ring
	ev, ok := ring.Pop()
	if !ok {
		t.Fatalf("expected event in ring buffer")
	}
	if ev.Kind != 2 { // Snapshot
		t.Fatalf("expected event kind 2 (Snapshot), got %d", ev.Kind)
	}
	expectedMid := (65000.00 + 65000.50) * 0.5
	if ev.Price != expectedMid {
		t.Fatalf("expected mid price %f, got %f", expectedMid, ev.Price)
	}
	if ev.Flags&VenueBybit == 0 {
		t.Fatalf("expected VenueBybit flag set, got %d", ev.Flags)
	}

	// Feed trade payload
	ingestor.HandleStreamPayload(bybitSampleTrades)

	// Check trade 1
	tr1, ok := ring.Pop()
	if !ok || tr1.Kind != 0 || tr1.Price != 65000.50 || tr1.Side != 1 {
		t.Fatalf("unexpected trade 1 in ring: %v, ok=%v", tr1, ok)
	}
	// Check trade 2
	tr2, ok := ring.Pop()
	if !ok || tr2.Kind != 0 || tr2.Price != 65000.00 || tr2.Side != 2 {
		t.Fatalf("unexpected trade 2 in ring: %v, ok=%v", tr2, ok)
	}

	// Verify L2 book clone
	book, exists := ingestor.GetL2Book("BTCUSDT")
	if !exists || book == nil {
		t.Fatalf("expected BTCUSDT book to exist")
	}
	mid, hasMid := book.MidPrice()
	if !hasMid || mid != expectedMid {
		t.Fatalf("expected book mid %f, got %f", expectedMid, mid)
	}
}

func TestDualVenueIngestorCreation(t *testing.T) {
	ring := engine.NewSPSCRing(1024)
	binanceCfg := DefaultStreamConfig()
	bybitCfg := DefaultBybitStreamConfig()

	dual := NewDualVenueIngestor(binanceCfg, bybitCfg, ring)
	if dual.Binance() == nil || dual.Bybit() == nil {
		t.Fatalf("expected non-nil child ingestors")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start and immediate stop
	dual.Start(ctx)
	time.Sleep(10 * time.Millisecond)
	dual.Stop()
}

func BenchmarkBybitFastParseOrderbook(b *testing.B) {
	var delta marketdata.OrderBookDelta
	var isSnapshot bool

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FastParseBybitOrderbook(bybitSampleSnapshot, &delta, &isSnapshot)
	}
}

func BenchmarkBybitFastParseTrades(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FastParseBybitTrades(bybitSampleTrades, func(p, q float64, s uint8, t int64) {})
	}
}

func BenchmarkBybitFastExtractSymbol(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FastExtractBybitSymbol(bybitSampleSnapshot)
	}
}
