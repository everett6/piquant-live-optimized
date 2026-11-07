package marketdata

import (
	"encoding/json"
	"testing"
)

var (
	rawDepthPayload = []byte(`{"lastUpdateId":24785532057,"bids":[["67250.10","0.450"],["67250.00","1.200"],["67249.50","0.850"],["67249.00","2.100"],["67248.50","0.320"]],"asks":[["67250.20","1.200"],["67250.50","0.750"],["67251.00","3.400"],["67251.50","0.150"],["67252.00","1.800"]]}`)

	combinedDepthPayload = []byte(`{"stream":"btcusdt@depth20@100ms","data":{"lastUpdateId":24785532057,"bids":[["67250.10","0.450"],["67250.00","1.200"]],"asks":[["67250.20","1.200"],["67250.50","0.750"]]}}`)

	diffDepthPayload = []byte(`{"e":"depthUpdate","E":1672531199000,"s":"BTCUSDT","U":24785532000,"u":24785532057,"pu":24785531999,"b":[["67250.10","0.450"]],"a":[["67250.20","1.200"]]}`)

	rawTradePayload = []byte(`{"e":"trade","E":1672531199000,"s":"BTCUSDT","t":1234567,"p":"67250.10","q":"0.050","T":1672531198990,"m":true,"M":true}`)

	combinedTradePayload = []byte(`{"stream":"btcusdt@trade","data":{"e":"trade","E":1672531199000,"s":"BTCUSDT","t":1234567,"p":"67250.10","q":"0.050","T":1672531198990,"m":false,"M":true}}`)
)

func TestFastParseDepth20_Raw(t *testing.T) {
	var delta OrderBookDelta
	ok := FastParseDepth20(rawDepthPayload, &delta)
	if !ok {
		t.Fatalf("FastParseDepth20 raw failed")
	}
	if delta.u != 24785532057 {
		t.Errorf("expected u=24785532057, got %d", delta.u)
	}
	if len(delta.Bids) != 5 || len(delta.Asks) != 5 {
		t.Fatalf("expected 5 bids and 5 asks, got %d bids and %d asks", len(delta.Bids), len(delta.Asks))
	}
	if delta.Bids[0].Price != 67250.10 || delta.Bids[0].Size != 0.450 {
		t.Errorf("unexpected top bid: %+v", delta.Bids[0])
	}
	if delta.Asks[0].Price != 67250.20 || delta.Asks[0].Size != 1.200 {
		t.Errorf("unexpected top ask: %+v", delta.Asks[0])
	}
}

func TestFastParseDepth20_Combined(t *testing.T) {
	var delta OrderBookDelta
	ok := FastParseDepth20(combinedDepthPayload, &delta)
	if !ok {
		t.Fatalf("FastParseDepth20 combined failed")
	}
	if delta.u != 24785532057 {
		t.Errorf("expected u=24785532057, got %d", delta.u)
	}
	if len(delta.Bids) != 2 || len(delta.Asks) != 2 {
		t.Fatalf("expected 2 bids and 2 asks, got %d and %d", len(delta.Bids), len(delta.Asks))
	}
}

func TestFastParseDepth20_Diff(t *testing.T) {
	var delta OrderBookDelta
	ok := FastParseDepth20(diffDepthPayload, &delta)
	if !ok {
		t.Fatalf("FastParseDepth20 diff failed")
	}
	if delta.U != 24785532000 || delta.u != 24785532057 || delta.pu != 24785531999 {
		t.Errorf("unexpected update IDs: U=%d u=%d pu=%d", delta.U, delta.u, delta.pu)
	}
	if len(delta.Bids) != 1 || delta.Bids[0].Price != 67250.10 {
		t.Errorf("unexpected bids: %+v", delta.Bids)
	}
}

func TestFastParseTrade_Raw(t *testing.T) {
	var (
		symbol       string
		price        float64
		qty          float64
		tradeTime    int64
		isBuyerMaker bool
	)
	ok := FastParseTrade(rawTradePayload, &symbol, &price, &qty, &tradeTime, &isBuyerMaker)
	if !ok {
		t.Fatalf("FastParseTrade raw failed")
	}
	if symbol != "BTCUSDT" {
		t.Errorf("expected symbol=BTCUSDT, got %s", symbol)
	}
	if price != 67250.10 {
		t.Errorf("expected price=67250.10, got %f", price)
	}
	if qty != 0.050 {
		t.Errorf("expected qty=0.050, got %f", qty)
	}
	if tradeTime != 1672531198990 {
		t.Errorf("expected tradeTime=1672531198990, got %d", tradeTime)
	}
	if !isBuyerMaker {
		t.Errorf("expected isBuyerMaker=true, got %v", isBuyerMaker)
	}
}

func TestFastParseTrade_Combined(t *testing.T) {
	var (
		symbol       string
		price        float64
		qty          float64
		tradeTime    int64
		isBuyerMaker bool
	)
	ok := FastParseTrade(combinedTradePayload, &symbol, &price, &qty, &tradeTime, &isBuyerMaker)
	if !ok {
		t.Fatalf("FastParseTrade combined failed")
	}
	if symbol != "BTCUSDT" {
		t.Errorf("expected symbol=BTCUSDT, got %s", symbol)
	}
	if price != 67250.10 {
		t.Errorf("expected price=67250.10, got %f", price)
	}
	if isBuyerMaker {
		t.Errorf("expected isBuyerMaker=false, got %v", isBuyerMaker)
	}
}

func BenchmarkFastParseDepth20(b *testing.B) {
	var delta OrderBookDelta
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !FastParseDepth20(rawDepthPayload, &delta) {
			b.Fatal("parse failed")
		}
	}
}

func BenchmarkFastParseDepth20_Combined(b *testing.B) {
	var delta OrderBookDelta
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !FastParseDepth20(combinedDepthPayload, &delta) {
			b.Fatal("parse failed")
		}
	}
}

func BenchmarkStdlibJSONDepth20(b *testing.B) {
	type stdDepthPayload struct {
		LastUpdateID uint64     `json:"lastUpdateId"`
		Bids         [][]string `json:"bids"`
		Asks         [][]string `json:"asks"`
	}
	var out stdDepthPayload
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := json.Unmarshal(rawDepthPayload, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFastParseTrade(b *testing.B) {
	var (
		symbol       string
		price        float64
		qty          float64
		tradeTime    int64
		isBuyerMaker bool
	)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !FastParseTrade(rawTradePayload, &symbol, &price, &qty, &tradeTime, &isBuyerMaker) {
			b.Fatal("parse failed")
		}
	}
}

func BenchmarkFastParseTrade_Combined(b *testing.B) {
	var (
		symbol       string
		price        float64
		qty          float64
		tradeTime    int64
		isBuyerMaker bool
	)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !FastParseTrade(combinedTradePayload, &symbol, &price, &qty, &tradeTime, &isBuyerMaker) {
			b.Fatal("parse failed")
		}
	}
}

func BenchmarkStdlibJSONTrade(b *testing.B) {
	type stdTradePayload struct {
		Symbol    string `json:"s"`
		Price     string `json:"p"`
		Quantity  string `json:"q"`
		TradeTime int64  `json:"T"`
		IsBuyerMM bool   `json:"m"`
	}
	var out stdTradePayload
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := json.Unmarshal(rawTradePayload, &out); err != nil {
			b.Fatal(err)
		}
	}
}
