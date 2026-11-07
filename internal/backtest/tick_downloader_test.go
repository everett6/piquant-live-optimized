package backtest

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"piquant-live/internal/event"
)

func TestParseAggTradeCSV(t *testing.T) {
	csvData := `agg_trade_id,price,quantity,first_trade_id,last_trade_id,transact_time,is_buyer_maker,is_best_match
2807759891,42283.58000000,0.00069000,3344774379,3344774379,1704067200000,True,True
2807759892,42283.59000000,0.00144000,3344774380,3344774380,1704067200001,False,True
2807759893,42283.58000000,0.00102000,3344774381,3344774381,1704067200003,True,False
`

	var ticks []AggTradeTick
	err := ParseAggTradeCSV(strings.NewReader(csvData), func(tick *AggTradeTick) error {
		ticks = append(ticks, *tick)
		return nil
	})
	if err != nil {
		t.Fatalf("ParseAggTradeCSV failed: %v", err)
	}

	if len(ticks) != 3 {
		t.Fatalf("expected 3 ticks, got %d", len(ticks))
	}

	// Verify tick 0
	t0 := ticks[0]
	if t0.TradeID != 2807759891 {
		t.Errorf("tick 0 TradeID = %d, want 2807759891", t0.TradeID)
	}
	if t0.Price != 42283.58 {
		t.Errorf("tick 0 Price = %f, want 42283.58", t0.Price)
	}
	if t0.Quantity != 0.00069 {
		t.Errorf("tick 0 Quantity = %f, want 0.00069", t0.Quantity)
	}
	if t0.FirstTradeID != 3344774379 || t0.LastTradeID != 3344774379 {
		t.Errorf("tick 0 first/last trade id mismatch")
	}
	if t0.TimestampNs != 1704067200000*1_000_000 {
		t.Errorf("tick 0 TimestampNs = %d", t0.TimestampNs)
	}
	if !t0.IsBuyerMaker || !t0.IsBestMatch {
		t.Errorf("tick 0 flags mismatch: IsBuyerMaker=%t, IsBestMatch=%t", t0.IsBuyerMaker, t0.IsBestMatch)
	}
	if t0.AggressorSide() != event.SideSell {
		t.Errorf("tick 0 AggressorSide = %v, want SideSell", t0.AggressorSide())
	}

	// Verify tick 1 (buyer aggressor)
	t1 := ticks[1]
	if t1.IsBuyerMaker {
		t.Errorf("tick 1 IsBuyerMaker should be false")
	}
	if t1.AggressorSide() != event.SideBuy {
		t.Errorf("tick 1 AggressorSide = %v, want SideBuy", t1.AggressorSide())
	}
}

func TestPQBinSerialization(t *testing.T) {
	ticks := []AggTradeTick{
		{
			TradeID:      10001,
			Price:        50123.45,
			Quantity:     0.1234,
			FirstTradeID: 20001,
			LastTradeID:  20002,
			TimestampNs:  1700000000000000000,
			IsBuyerMaker: true,
			IsBestMatch:  true,
		},
		{
			TradeID:      10002,
			Price:        50124.50,
			Quantity:     0.5678,
			FirstTradeID: 20003,
			LastTradeID:  20003,
			TimestampNs:  1700000000005000000,
			IsBuyerMaker: false,
			IsBestMatch:  true,
		},
	}

	var buf bytes.Buffer
	// Test header and tick saving
	tmpFile := t.TempDir() + "/test.pqbin"
	if err := SavePQBin(tmpFile, "BTCUSDT", 2024, 1, ticks); err != nil {
		t.Fatalf("SavePQBin failed: %v", err)
	}

	var loadedTicks []AggTradeTick
	hdr, err := LoadPQBinFile(tmpFile, func(tick *AggTradeTick) error {
		loadedTicks = append(loadedTicks, *tick)
		return nil
	})
	if err != nil {
		t.Fatalf("LoadPQBinFile failed: %v", err)
	}

	if hdr.SymbolStr() != "BTCUSDT" {
		t.Errorf("hdr symbol = %s, want BTCUSDT", hdr.SymbolStr())
	}
	if hdr.Year != 2024 || hdr.Month != 1 {
		t.Errorf("hdr year/month mismatch: %d-%d", hdr.Year, hdr.Month)
	}
	if hdr.TickCount != 2 {
		t.Errorf("hdr tickCount = %d, want 2", hdr.TickCount)
	}

	if len(loadedTicks) != 2 {
		t.Fatalf("expected 2 loaded ticks, got %d", len(loadedTicks))
	}

	for i := range ticks {
		lt := loadedTicks[i]
		ot := ticks[i]
		if lt.TradeID != ot.TradeID {
			t.Errorf("tick %d TradeID = %d, want %d", i, lt.TradeID, ot.TradeID)
		}
		if math.Abs(lt.Price-ot.Price) > 1e-4 {
			t.Errorf("tick %d Price = %f, want %f", i, lt.Price, ot.Price)
		}
		if math.Abs(lt.Quantity-ot.Quantity) > 1e-4 {
			t.Errorf("tick %d Quantity = %f, want %f", i, lt.Quantity, ot.Quantity)
		}
		if lt.TimestampNs != ot.TimestampNs {
			t.Errorf("tick %d TimestampNs = %d, want %d", i, lt.TimestampNs, ot.TimestampNs)
		}
		if lt.IsBuyerMaker != ot.IsBuyerMaker {
			t.Errorf("tick %d IsBuyerMaker = %t, want %t", i, lt.IsBuyerMaker, ot.IsBuyerMaker)
		}
	}
	_ = buf
}

func TestToMarketEventAndEnvelope(t *testing.T) {
	tick := AggTradeTick{
		TradeID:      123456,
		Price:        45000.50,
		Quantity:     0.05,
		TimestampNs:  1700000000000000000,
		IsBuyerMaker: true, // Sell aggressor
	}

	ev := tick.ToMarketEvent()
	if ev.Sequence != 123456 || ev.Price != 45000.50 || ev.Size != 0.05 || ev.Side != 2 {
		t.Errorf("ToMarketEvent mismatch: %+v", ev)
	}

	env := tick.ToEnvelope()
	if env.Sequence != 123456 || env.Kind != event.KindTrade || env.Trade.Side != event.SideSell {
		t.Errorf("ToEnvelope mismatch: %+v", env)
	}
}

func TestTickDownloaderURLs(t *testing.T) {
	dl := NewTickDownloader(t.TempDir())
	mUrl := dl.MonthlyZipURL("btcusdt", 2024, 1)
	expectedM := "https://data.binance.vision/data/spot/monthly/aggTrades/BTCUSDT/BTCUSDT-aggTrades-2024-01.zip"
	if mUrl != expectedM {
		t.Errorf("MonthlyZipURL = %s, want %s", mUrl, expectedM)
	}

	dUrl := dl.DailyZipURL("ETHUSDT", 2024, 1, 15)
	expectedD := "https://data.binance.vision/data/spot/daily/aggTrades/ETHUSDT/ETHUSDT-aggTrades-2024-01-15.zip"
	if dUrl != expectedD {
		t.Errorf("DailyZipURL = %s, want %s", dUrl, expectedD)
	}
}

func BenchmarkParseAggTradeCSV_HotLoop(b *testing.B) {
	csvLine := "2807759891,42283.58000000,0.00069000,3344774379,3344774379,1704067200000,True,True\n"
	data := strings.Repeat(csvLine, 1000)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		r := strings.NewReader(data)
		_ = ParseAggTradeCSV(r, func(tick *AggTradeTick) error {
			_ = tick.TradeID
			return nil
		})
	}
}
