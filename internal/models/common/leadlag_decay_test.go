package common

import (
	"math"
	"testing"
)

func TestCrossVenueLeadLagTracker(t *testing.T) {
	tracker := NewCrossVenueLeadLagTracker("Binance", "Bybit")

	nowNs := int64(1700000000000000000)
	// Initial update
	sig1 := tracker.UpdatePrices(50000.0, 50000.0, nowNs, nowNs)
	if sig1.LeadLagSpreadBps != 0.0 {
		t.Fatalf("expected 0 spread bps, got %f", sig1.LeadLagSpreadBps)
	}

	// Venue A leads with a sudden upward innovation of +50.0 (10 bps), Venue B lags
	sig2 := tracker.UpdatePrices(50050.0, 50000.0, nowNs+100_000_000, nowNs+85_000_000)
	if sig2.LeadLagSpreadBps <= 0 {
		t.Fatalf("expected positive spread bps, got %f", sig2.LeadLagSpreadBps)
	}
	if sig2.LeaderVenue != "Binance" {
		t.Fatalf("expected Binance to lead, got %s", sig2.LeaderVenue)
	}
	if sig2.FollowerVenue != "Bybit" {
		t.Fatalf("expected Bybit to follow, got %s", sig2.FollowerVenue)
	}
	if sig2.PredictedDriftBps <= 0 {
		t.Fatalf("expected positive predicted drift for follower, got %f", sig2.PredictedDriftBps)
	}
	if sig2.Confidence <= 0 || sig2.Confidence > 1.0 {
		t.Fatalf("invalid confidence: %f", sig2.Confidence)
	}

	// Update single venue tick
	sig3 := tracker.UpdateVenueTick("Bybit", 50045.0, nowNs+120_000_000)
	if sig3.LeadLagSpreadBps >= sig2.LeadLagSpreadBps {
		t.Fatalf("expected spread to compress after follower update, got %f", sig3.LeadLagSpreadBps)
	}

	// Test Predict with feature vector
	var x [FeatureCount]float64
	x[3] = 0.25 // micro_dev
	x[4] = 0.80 // spread_bps
	x[5] = 1.20 // ofi_1s
	pred := tracker.Predict(&x)
	if pred == 0 {
		t.Fatalf("expected non-zero prediction, got %f", pred)
	}

	// Test Reset
	tracker.Reset()
	if tracker.PriceA != 0 || tracker.PriceB != 0 {
		t.Fatalf("expected reset prices, got %f, %f", tracker.PriceA, tracker.PriceB)
	}
}

func TestInventoryAgingPolicy(t *testing.T) {
	policy := NewInventoryAgingPolicy(1.5, 30.0, 1.5)

	bid := 49999.50
	ask := 50000.50
	res := 50000.00

	// 1. Flat inventory: quotes should not change
	adjBid0, adjAsk0, adjRes0, urg0 := policy.AdjustQuotes(bid, ask, res, 0.0, 45.0)
	if adjBid0 != bid || adjAsk0 != ask || adjRes0 != res || urg0 != 0.0 {
		t.Fatalf("expected unchanged quotes on 0 inventory, got bid=%f ask=%f res=%f urg=%f", adjBid0, adjAsk0, adjRes0, urg0)
	}

	// 2. Long position q = 1.0 held for 10s (nominal)
	adjBid10, adjAsk10, adjRes10, urg10 := policy.AdjustQuotes(bid, ask, res, 1.0, 10.0)
	if adjRes10 >= res {
		t.Fatalf("expected reservation price to drop for long position, got %f vs %f", adjRes10, res)
	}
	if adjAsk10 >= ask {
		t.Fatalf("expected ask quote to discount for long position, got %f vs %f", adjAsk10, ask)
	}
	if adjBid10 >= bid {
		t.Fatalf("expected bid quote to drop for long position, got %f vs %f", adjBid10, bid)
	}
	if urg10 <= 0 {
		t.Fatalf("expected positive urgency bps, got %f", urg10)
	}

	// 3. Long position held for 45s (>30s stale)
	_, adjAsk45, _, urg45 := policy.AdjustQuotes(bid, ask, res, 1.0, 45.0)
	if urg45 <= urg10 {
		t.Fatalf("expected urgency to increase with age: urg45=%f <= urg10=%f", urg45, urg10)
	}
	if adjAsk45 >= adjAsk10 {
		t.Fatalf("expected ask to narrow further at 45s: %f >= %f", adjAsk45, adjAsk10)
	}

	// 4. Long position held for 75s (>60s critical liquidation)
	_, adjAsk75, adjRes75, urg75 := policy.AdjustQuotes(bid, ask, res, 1.0, 75.0)
	if urg75 <= urg45 {
		t.Fatalf("expected higher urgency at 75s: %f <= %f", urg75, urg45)
	}
	if adjAsk75 > adjRes75+0.5 {
		t.Fatalf("expected aggressive liquidation near reservation, ask=%f res=%f", adjAsk75, adjRes75)
	}

	// 5. Short position q = -1.0
	adjBidShort, adjAskShort, adjResShort, urgShort := policy.AdjustQuotes(bid, ask, res, -1.0, 50.0)
	if adjResShort <= res {
		t.Fatalf("expected reservation price to rise for short position, got %f vs %f", adjResShort, res)
	}
	if adjBidShort <= bid {
		t.Fatalf("expected bid quote to rise (bid up to cover) for short, got %f vs %f", adjBidShort, bid)
	}
	if adjAskShort <= ask {
		t.Fatalf("expected ask quote to rise for short, got %f vs %f", adjAskShort, ask)
	}
	if urgShort <= 0 {
		t.Fatalf("expected positive urgency bps for short, got %f", urgShort)
	}

	// 6. Safety invariant: adjBid < adjAsk
	if adjBidShort >= adjAskShort {
		t.Fatalf("sanity invariant violated: bid %f >= ask %f", adjBidShort, adjAskShort)
	}
}

func TestInventoryAgingPolicyPositionTracking(t *testing.T) {
	policy := NewInventoryAgingPolicy(1.5, 30.0, 1.5)

	t0 := int64(100_000_000_000)
	// Open long 1.0
	policy.OnFill(1.0, 50000.0, t0)
	if policy.Inventory != 1.0 || policy.EntryTimeNs != t0 {
		t.Fatalf("initial fill failed: inv=%f entry=%d", policy.Inventory, policy.EntryTimeNs)
	}

	// Add 0.5 long at t0 + 10s: entry timestamp must be preserved
	t1 := t0 + 10_000_000_000
	policy.OnFill(0.5, 50010.0, t1)
	if policy.Inventory != 1.5 || policy.EntryTimeNs != t0 {
		t.Fatalf("add fill failed: inv=%f entry=%d (want %d)", policy.Inventory, policy.EntryTimeNs, t0)
	}
	dur := policy.HoldingDurationSec(t1)
	if math.Abs(dur-10.0) > 1e-6 {
		t.Fatalf("expected 10s holding duration, got %f", dur)
	}

	// Partial sale -0.5: entry timestamp still preserved
	t2 := t1 + 5_000_000_000
	policy.OnFill(-0.5, 50020.0, t2)
	if policy.Inventory != 1.0 || policy.EntryTimeNs != t0 {
		t.Fatalf("partial sell failed: inv=%f entry=%d (want %d)", policy.Inventory, policy.EntryTimeNs, t0)
	}

	// Close remaining -1.0: flattened
	t3 := t2 + 5_000_000_000
	policy.OnFill(-1.0, 50020.0, t3)
	if policy.Inventory != 0.0 || policy.EntryTimeNs != 0 {
		t.Fatalf("flattening failed: inv=%f entry=%d", policy.Inventory, policy.EntryTimeNs)
	}
	if policy.HoldingDurationSec(t3) != 0.0 {
		t.Fatalf("expected 0 duration when flat, got %f", policy.HoldingDurationSec(t3))
	}

	// Flip to short
	t4 := t3 + 1_000_000_000
	policy.OnFill(-2.0, 50000.0, t4)
	if policy.Inventory != -2.0 || policy.EntryTimeNs != t4 {
		t.Fatalf("flip to short failed: inv=%f entry=%d (want %d)", policy.Inventory, policy.EntryTimeNs, t4)
	}
}

func BenchmarkCrossVenueLeadLagUpdate(b *testing.B) {
	tracker := NewCrossVenueLeadLagTracker("Binance", "Bybit")
	tsA := int64(1700000000000000000)
	tsB := tsA - 15_000_000

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pA := 50000.0 + float64(i%20)*0.1
		pB := 49999.5 + float64(i%15)*0.1
		tracker.UpdatePrices(pA, pB, tsA, tsB)
	}
}

func BenchmarkCrossVenueLeadLagPredict(b *testing.B) {
	tracker := NewCrossVenueLeadLagTracker("Binance", "Bybit")
	var x [FeatureCount]float64
	x[3] = 0.25
	x[4] = 0.80
	x[5] = 1.20

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tracker.Predict(&x)
	}
}

func BenchmarkInventoryAgingAdjustQuotes(b *testing.B) {
	policy := NewInventoryAgingPolicy(1.5, 30.0, 1.5)
	bid := 49999.50
	ask := 50000.50
	res := 50000.00
	inv := 0.75
	dur := 42.5

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = policy.AdjustQuotes(bid, ask, res, inv, dur)
	}
}

func BenchmarkInventoryAgingUrgencyPenalty(b *testing.B) {
	policy := NewInventoryAgingPolicy(1.5, 30.0, 1.5)
	inv := 0.75
	dur := 42.5

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = policy.UrgencyPenalty(inv, dur)
	}
}
