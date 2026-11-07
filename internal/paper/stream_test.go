package paper

import (
	"encoding/json"
	"testing"
)

type fullTradePayload struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	TradeID   uint64 `json:"t"`
	Price     string `json:"p"`
	Quantity  string `json:"q"`
	TradeTime int64  `json:"T"`
	IsBuyerMM bool   `json:"m"`
	Ignore    bool   `json:"M"`
}

func TestTradePayloadUnmarshal(t *testing.T) {
	payload := []byte(`{"stream":"btcusdt@trade","data":{"e":"trade","E":1793862063463,"s":"BTCUSDT","t":6821515870,"p":"80626.01000000","q":"0.00056000","T":1793862063463,"m":false,"M":true}}`)

	var combined binanceCombinedStreamPayload
	if err := json.Unmarshal(payload, &combined); err != nil {
		t.Fatalf("combined unmarshal failed: %v", err)
	}

	var tr fullTradePayload
	if err := json.Unmarshal(combined.Data, &tr); err != nil {
		t.Fatalf("trade unmarshal failed: %v", err)
	}
	t.Logf("SUCCESS: event=%q time=%d symbol=%q price=%s qty=%s id=%d isBuyerMM=%v",
		tr.EventType, tr.EventTime, tr.Symbol, tr.Price, tr.Quantity, tr.TradeID, tr.IsBuyerMM)
	if tr.TradeID != 6821515870 {
		t.Fatalf("TradeID mismatch: got %d, want 6821515870", tr.TradeID)
	}
	if tr.IsBuyerMM != false {
		t.Fatalf("IsBuyerMM mismatch: got %v, want false", tr.IsBuyerMM)
	}
}
