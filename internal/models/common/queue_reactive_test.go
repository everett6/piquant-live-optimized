package common

import (
	"testing"
)

func TestQueueReactiveModel(t *testing.T) {
	qm := NewQueueReactiveModel()

	// Test bin classifications
	if b := qm.QueueBin(1.0); b != 0 {
		t.Errorf("expected bin 0, got %d", b)
	}
	if b := qm.QueueBin(3.5); b != 1 {
		t.Errorf("expected bin 1, got %d", b)
	}
	if b := qm.QueueBin(10.0); b != 2 {
		t.Errorf("expected bin 2, got %d", b)
	}
	if b := qm.QueueBin(50.0); b != 3 {
		t.Errorf("expected bin 3, got %d", b)
	}

	// Test monotonic FillProbability with horizon
	p1s := qm.FillProbabilityFor(1.0, 5.0, 0.0)
	p5s := qm.FillProbabilityFor(5.0, 5.0, 0.0)
	p15s := qm.FillProbabilityFor(15.0, 5.0, 0.0)

	if p1s <= 0 || p1s >= p5s || p5s >= p15s || p15s > 1.0 {
		t.Errorf("expected monotonic fill probabilities: p1s=%.4f p5s=%.4f p15s=%.4f", p1s, p5s, p15s)
	}

	// Test queue ahead impact: larger queue ahead -> lower fill probability
	pSmallQueue := qm.FillProbabilityFor(5.0, 1.0, 0.0)
	pLargeQueue := qm.FillProbabilityFor(5.0, 20.0, 0.0)
	if pSmallQueue <= pLargeQueue {
		t.Errorf("expected small queue to fill faster than large queue: small=%.4f large=%.4f", pSmallQueue, pLargeQueue)
	}

	// Test online update
	oldTradeRate := qm.TradeRates[0]
	qm.UpdateOnline(0, 10.0, 1.0, 0.5) // 20 units/sec trade sample in bin 0
	if qm.TradeRates[0] <= oldTradeRate {
		t.Errorf("expected trade rate to increase after high volume trade update: old=%.2f new=%.2f", oldTradeRate, qm.TradeRates[0])
	}
}

func BenchmarkQueueReactiveModel_FillProbability(b *testing.B) {
	qm := NewQueueReactiveModel()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		p := qm.FillProbabilityFor(5.0, 2.5, -0.5)
		if p <= 0 {
			b.Fatal("unexpected p")
		}
	}
}
