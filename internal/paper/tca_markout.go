package paper

// TCAMarkoutRingCapacity is the fixed power-of-two ring capacity for tracking in-flight fills.
const TCAMarkoutRingCapacity = 256

// Multi-horizon durations in nanoseconds.
const (
	Horizon1sNs  int64 = 1_000_000_000
	Horizon5sNs  int64 = 5_000_000_000
	Horizon10sNs int64 = 10_000_000_000
	Horizon60sNs int64 = 60_000_000_000
)

// PendingMarkout stores execution price and multi-horizon post-trade markouts for one fill.
// Fixed-size structure allocated in place with zero heap allocations.
type PendingMarkout struct {
	FillID           uint64
	Symbol           [16]byte // Fixed-size byte array to avoid string heap escape
	Side             int8     // +1 for BUY, -1 for SELL
	FillPrice        float64
	MidAtFill        float64
	FillTimeNs       int64
	SpreadCaptureBps float64
	Markout1sBps     float64
	Markout5sBps     float64
	Markout10sBps    float64
	Markout60sBps    float64
	Done1s           bool
	Done5s           bool
	Done10s          bool
	Done60s          bool
	Active           bool
}

// TCAMarkoutSnapshot provides an instantaneous snapshot of institutional TCA execution metrics.
type TCAMarkoutSnapshot struct {
	TotalFillsAnalyzed   int64   `json:"total_fills_analyzed"`
	MeanSpreadCaptureBps float64 `json:"spread_capture_bps"`
	MeanMarkout1sBps     float64 `json:"markout_1s_bps"`
	MeanMarkout5sBps     float64 `json:"markout_5s_bps"`
	MeanMarkout10sBps    float64 `json:"markout_10s_bps"`
	MeanMarkout60sBps    float64 `json:"markout_60s_bps"`
	AdverseSelectionBps  float64 `json:"adverse_selection_bps"`
	ToxicFillRatio       float64 `json:"toxic_fill_ratio"`
}

// TCAMarkoutTracker tracks executed fills and computes post-trade multi-horizon price markouts
// and execution edge decomposition in a fixed ring buffer without heap allocations.
type TCAMarkoutTracker struct {
	Ring  [TCAMarkoutRingCapacity]PendingMarkout
	Head  uint32
	Count uint32

	// Cumulative statistics across evaluated fills
	TotalFills          int64
	SumSpreadCaptureBps float64
	Count1s             int64
	SumMarkout1sBps     float64
	Count5s             int64
	SumMarkout5sBps     float64
	Count10s            int64
	SumMarkout10sBps    float64
	Count60s            int64
	SumMarkout60sBps    float64
	ToxicFillsCount     int64
}

// NewTCAMarkoutTracker constructs an empty TCAMarkoutTracker.
func NewTCAMarkoutTracker() *TCAMarkoutTracker {
	return &TCAMarkoutTracker{}
}

// RecordFill records an executed fill into the fixed ring buffer and computes immediate spread capture.
//
// Formula:
//
//	SpreadCapture = side * ((Mid - FillPrice) / FillPrice) * 10,000 bps
//
// Hot path: 0 B/op, 0 allocs/op.
func (t *TCAMarkoutTracker) RecordFill(fillID uint64, symbol string, side int8, fillPrice, midPrice float64, fillTimeNs int64) {
	if fillPrice <= 0 || midPrice <= 0 {
		return
	}

	idx := t.Head & (TCAMarkoutRingCapacity - 1)
	t.Head++
	t.Count++

	entry := &t.Ring[idx]
	entry.FillID = fillID

	var sym [16]byte
	copy(sym[:], symbol)
	entry.Symbol = sym

	entry.Side = side
	entry.FillPrice = fillPrice
	entry.MidAtFill = midPrice
	entry.FillTimeNs = fillTimeNs

	spreadCapture := float64(side) * ((midPrice - fillPrice) / fillPrice) * 10000.0
	entry.SpreadCaptureBps = spreadCapture
	entry.Markout1sBps = 0
	entry.Markout5sBps = 0
	entry.Markout10sBps = 0
	entry.Markout60sBps = 0
	entry.Done1s = false
	entry.Done5s = false
	entry.Done10s = false
	entry.Done60s = false
	entry.Active = true

	t.TotalFills++
	t.SumSpreadCaptureBps += spreadCapture
}

// OnTick updates pending markouts across all 4 horizons (+1s, +5s, +10s, +60s) for the given symbol.
//
// Formula:
//
//	Markout_\tau = side * ((Mid_{t+\tau} - FillPrice_t) / FillPrice_t) * 10,000 bps
//
// Hot path: 0 B/op, 0 allocs/op.
func (t *TCAMarkoutTracker) OnTick(symbol string, midPrice float64, tickTimeNs int64) {
	if midPrice <= 0 {
		return
	}

	var sym [16]byte
	copy(sym[:], symbol)

	for i := 0; i < TCAMarkoutRingCapacity; i++ {
		entry := &t.Ring[i]
		if !entry.Active || entry.Symbol != sym {
			continue
		}

		elapsed := tickTimeNs - entry.FillTimeNs
		if elapsed < 0 {
			continue
		}

		fillPrice := entry.FillPrice
		sideF := float64(entry.Side)

		if !entry.Done1s && elapsed >= Horizon1sNs {
			mo := sideF * ((midPrice - fillPrice) / fillPrice) * 10000.0
			entry.Markout1sBps = mo
			entry.Done1s = true
			t.Count1s++
			t.SumMarkout1sBps += mo
			if mo < -2.0 {
				t.ToxicFillsCount++
			}
		}

		if !entry.Done5s && elapsed >= Horizon5sNs {
			mo := sideF * ((midPrice - fillPrice) / fillPrice) * 10000.0
			entry.Markout5sBps = mo
			entry.Done5s = true
			t.Count5s++
			t.SumMarkout5sBps += mo
		}

		if !entry.Done10s && elapsed >= Horizon10sNs {
			mo := sideF * ((midPrice - fillPrice) / fillPrice) * 10000.0
			entry.Markout10sBps = mo
			entry.Done10s = true
			t.Count10s++
			t.SumMarkout10sBps += mo
		}

		if !entry.Done60s && elapsed >= Horizon60sNs {
			mo := sideF * ((midPrice - fillPrice) / fillPrice) * 10000.0
			entry.Markout60sBps = mo
			entry.Done60s = true
			t.Count60s++
			t.SumMarkout60sBps += mo
			entry.Active = false // All horizons complete
		}
	}
}

// Snapshot returns the current aggregate TCA execution and markout statistics.
//
// Hot path: 0 B/op, 0 allocs/op.
func (t *TCAMarkoutTracker) Snapshot() TCAMarkoutSnapshot {
	var snap TCAMarkoutSnapshot
	snap.TotalFillsAnalyzed = t.TotalFills

	if t.TotalFills > 0 {
		snap.MeanSpreadCaptureBps = t.SumSpreadCaptureBps / float64(t.TotalFills)
	}
	if t.Count1s > 0 {
		snap.MeanMarkout1sBps = t.SumMarkout1sBps / float64(t.Count1s)
		snap.ToxicFillRatio = float64(t.ToxicFillsCount) / float64(t.Count1s)
	}
	if t.Count5s > 0 {
		snap.MeanMarkout5sBps = t.SumMarkout5sBps / float64(t.Count5s)
	}
	if t.Count10s > 0 {
		snap.MeanMarkout10sBps = t.SumMarkout10sBps / float64(t.Count10s)
		snap.AdverseSelectionBps = snap.MeanSpreadCaptureBps - snap.MeanMarkout10sBps
	}
	if t.Count60s > 0 {
		snap.MeanMarkout60sBps = t.SumMarkout60sBps / float64(t.Count60s)
	}

	return snap
}
