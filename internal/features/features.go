// Package features turns book updates and trades into the model input vector
// without allocating. Book-derived quantities stay in fixed point int64 until
// the last step, where they are normalised to float32.
//
// # Time, not event count
//
// Every window is measured in event time (the receive timestamp carried in the
// envelope) and is assembled from fixed 50 ms time buckets, so a window always
// covers the period it claims to. The previous implementation kept the last 512
// events and summed those, which covered 5 s at 100 events/s but only 46 ms at
// 22 000 events/s — on the production feed the "30 s" OFI was really the last
// half-second of flow. Buckets make memory a function of the window length
// only: 1024 buckets (~51 s of headroom) per symbol, whatever the event rate.
//
// A window includes every sample in (t-W, t] except at most those in the one
// bucket that straddles t-W, which is dropped whole. It therefore never
// includes a sample older than the window claims; the cost is at most one
// bucket of quantisation (5% of a 1 s window, 0.17% of a 30 s window).
//
// # Consistent state
//
// Advance must be called AFTER the envelope has been applied to the book. All
// features of one vector then describe the same book state: the level features
// read the book directly, and the OFI terms are measured against the book state
// captured at the previous event.
package features

import (
	"math"

	"piquant-live/internal/event"
	"piquant-live/internal/orderbook"
)

// Names lists the feature vector layout. Keep in sync with NCore.
var Names = []string{
	"imb_l1",       // (bid_q - ask_q) / (bid_q + ask_q) over L1, [-1, 1], dimensionless
	"imb_l5",       // same over the top 5 levels
	"imb_l20",      // same over the top 20 levels
	"micro_dev",    // (microprice - mid) / spread, in [-0.5, 0.5]
	"spread_bps",   // (ask - bid) / mid * 1e4
	"ofi_1s",       // whole-book OFI over 1 s, base units (signed)
	"ofi_5s",       // whole-book OFI over 5 s
	"ofi_30s",      // whole-book OFI over 30 s
	"mlofi_1s",     // multi-level OFI, decay exp(-0.3*(level-1)), 1 s
	"mlofi_5s",     // multi-level OFI, same decay, 5 s
	"vol_30s_bps",  // std-dev of 1 s mid returns over 30 s, bps
	"ret_1s_bps",   // mid return over 1 s, bps
	"ret_5s_bps",   // mid return over 5 s, bps
	"trade_imb_5s", // (buy - sell) traded base units over 5 s
	"trade_cnt_1s", // number of trades in the last second
	"queue_ratio",  // best bid qty / (best bid qty + best ask qty), [0, 1]
}

// NCore is the number of features produced by each Extract call.
const NCore = 16

func init() {
	if len(Names) != NCore {
		panic("features: Names and NCore out of sync")
	}
}

const (
	// bucketNs is the aggregation width of every window.
	bucketNs = int64(50e6)

	// Window lengths in event-time nanoseconds.
	win1s  = int64(1e9)
	win5s  = int64(5e9)
	win30s = int64(30e9)

	// nBuckets is the bucket ring: 51.2 s of buckets, i.e. well past the
	// longest (30 s) window, so a bucket slot is never reused while any window
	// still holds it. Power of two for masking.
	nBuckets = 1024
	bktMask  = nBuckets - 1

	// maxMidScan bounds the backwards walk that finds the mid at or before a
	// reference time: 64 buckets = 3.2 s. Beyond that the book was idle across
	// the whole window and the return is reported as invalid (0) rather than
	// measured from a stale reference.
	maxMidScan = 64
)

// ofiBeta is the multi-level OFI decay across book levels.
const ofiBeta = 0.3

// bucket aggregates the windowed quantities of one 100 ms slice of event time.
type bucket struct {
	start    int64   // bucket start, ns
	ofi      float64 // whole-book OFI, base units
	mlofi    float64 // multi-level OFI, base units
	trade    float64 // signed traded base units
	tradeCnt int32   // trade count
	retN     int32   // number of 1 s return samples
	retSum   float64 // sum of those samples
	retSq    float64 // sum of their squares
	mid      int64   // last mid seen in the bucket
	midT     int64   // time of that mid
	hasMid   bool
}

// Engine holds per-symbol streaming state. One goroutine owns one Engine.
type Engine struct {
	sym uint16

	// Book state captured at the previous event, for measuring OFI and level
	// rank against the pre-update book once the current update is applied.
	prev    [2]orderbook.Levels
	prevMid int64
	prevOK  bool

	b       [nBuckets]bucket
	headSeq int64 // bucket sequence of the current bucket
	started bool

	// Window cursors: the oldest bucket sequence still included per window.
	c1, c5, c30 int64
	ofi1        float64
	ofi5        float64
	ofi30       float64
	ml1         float64
	ml5         float64
	tr1         float64
	tr5         float64
	cnt1        int64
	volN        int64
	volSum      float64
	volSq       float64

	// Novel Quant Algorithms state:
	// 1. Online Hawkes trade intensity clustering
	hawkesBuy   float64
	hawkesSell  float64
	hawkesLastT int64
	// 2. Volume-synchronized order flow toxicity (VPIN)
	vpinVolBucket float64
	vpinCurrBuy   float64
	vpinCurrSell  float64
	vpinDiffs     [8]float64
	vpinDiffIdx   int
	vpinFilled    int

	lastRecv  int64
	ready     bool
	emitted   bool
	reversals uint64 // events whose timestamp went backwards (data quality)
	emit      [NCore]float32
}

// New creates an engine for a symbol.
func New(sym uint16) *Engine { return &Engine{sym: sym} }

// Symbol returns the engine's symbol.
func (e *Engine) Symbol() uint16 { return e.sym }

// TimeReversals reports how many envelopes arrived with a timestamp older than
// the previous one. Such an event is folded into the current bucket rather
// than rewinding the windows, and the count is reported so a replay makes the
// data quality visible.
func (e *Engine) TimeReversals() uint64 { return e.reversals }

// Reset clears all state, e.g. after a book resync.
func (e *Engine) Reset() {
	sym := e.sym
	*e = Engine{sym: sym}
}

// Ready reports whether the last Advance saw a live book.
func (e *Engine) Ready() bool { return e.ready }

// Advance updates rolling state from one envelope. It must be called AFTER the
// envelope has been applied to the book: the level features and the mid sample
// then describe the state the update produced, and OFI is measured against the
// state captured before it.
func (e *Engine) Advance(book *orderbook.Book, env *event.Envelope) bool {
	t := env.RecvTime
	if t < e.lastRecv {
		e.reversals++
	} else {
		e.lastRecv = t
	}
	e.ready = book.Ready
	if e.ready && book.Anchored {
		// Move the bucket ring to t first: expire must see every bucket that
		// the streams skipped, not just the ones below the previous head.
		e.bucketFor(t)
		e.expire(t)
		switch env.Kind {
		case event.KindSnapshot:
			e.resetWindows(t)
			e.baseline(book, t)
		case event.KindDepth:
			e.update(book, &env.Depth, t)
		case event.KindTrade:
			e.trade(&env.Trade, t)
		}
		e.compute(book, t)
		switch env.Kind {
		case event.KindSnapshot, event.KindDepth:
			e.capturePrev(book)
		}
	} else {
		e.prevOK = false
	}
	return e.ready
}

// capturePrev stores the book state this event produced, which becomes the
// reference the next event's OFI is measured against.
func (e *Engine) capturePrev(book *orderbook.Book) {
	if !book.Ready || !book.Anchored {
		e.prevOK = false
		return
	}
	e.prev[orderbook.Bids] = book.Levels[orderbook.Bids]
	e.prev[orderbook.Asks] = book.Levels[orderbook.Asks]
	bp, _ := e.prev[orderbook.Bids].Best()
	ap, _ := e.prev[orderbook.Asks].Best()
	e.prevMid = (bp + ap) / 2
	e.prevOK = true
}

// bucketFor returns the current bucket, advancing the head (clearing the
// buckets skipped over) when event time has moved on. A backwards timestamp is
// folded into the current bucket.
func (e *Engine) bucketFor(t int64) *bucket {
	k := t / bucketNs
	if !e.started {
		e.resetWindows(t)
		return &e.b[k&bktMask]
	}
	if k <= e.headSeq {
		return &e.b[e.headSeq&bktMask]
	}
	if k-e.headSeq >= nBuckets {
		// More than the whole retention horizon passed: nothing in any window
		// survives, so start clean instead of walking the ring.
		e.resetWindows(t)
		return &e.b[k&bktMask]
	}
	for s := e.headSeq + 1; s <= k; s++ {
		e.b[s&bktMask] = bucket{start: s * bucketNs}
	}
	e.headSeq = k
	return &e.b[k&bktMask]
}

// resetWindows forgets every sample and restarts the bucket ring at t.
func (e *Engine) resetWindows(t int64) {
	k := t / bucketNs
	e.b = [nBuckets]bucket{}
	e.b[k&bktMask] = bucket{start: k * bucketNs}
	e.headSeq = k
	e.started = true
	e.c1, e.c5, e.c30 = k, k, k
	e.ofi1, e.ofi5, e.ofi30 = 0, 0, 0
	e.ml1, e.ml5 = 0, 0
	e.tr1, e.tr5, e.cnt1 = 0, 0, 0
	e.volN, e.volSum, e.volSq = 0, 0, 0
}

// expire drops every bucket that has fallen out of a window. Each bucket is
// retired at most once per window, so expiry is O(1) amortised per event.
func (e *Engine) expire(t int64) {
	if !e.started {
		return
	}
	for e.c1 < e.headSeq && e.c1*bucketNs <= t-win1s {
		b := &e.b[e.c1&bktMask]
		e.ofi1 -= b.ofi
		e.ml1 -= b.mlofi
		e.tr1 -= b.trade
		e.cnt1 -= int64(b.tradeCnt)
		e.c1++
	}
	for e.c5 < e.headSeq && e.c5*bucketNs <= t-win5s {
		b := &e.b[e.c5&bktMask]
		e.ofi5 -= b.ofi
		e.ml5 -= b.mlofi
		e.tr5 -= b.trade
		e.c5++
	}
	for e.c30 < e.headSeq && e.c30*bucketNs <= t-win30s {
		b := &e.b[e.c30&bktMask]
		e.ofi30 -= b.ofi
		e.volN -= int64(b.retN)
		e.volSum -= b.retSum
		e.volSq -= b.retSq
		e.c30++
	}
}

// baseline records the post-snapshot book as a mid sample, so the first
// post-snapshot update has something to be measured against.
func (e *Engine) baseline(book *orderbook.Book, t int64) {
	bp, _ := book.Levels[orderbook.Bids].Best()
	ap, _ := book.Levels[orderbook.Asks].Best()
	b := e.bucketFor(t)
	b.mid, b.midT, b.hasMid = (bp+ap)/2, t, true
}

// update records the flow and the mid caused by one depth update.
//
// The per-level n-th term (OFI of Cont, Kukanov and Stoikov applied one price
// level at a time) is the size change at that level: on the bid, a level that
// did not exist before or was resized at the same price:
//
//	e_n = q_n            if the level is new
//	e_n = q_n - q_{n-1}  if the level is resized
//	e_n = 0              if the level is deleted (no impact at the touch)
//
// which, because prevQty is 0 for a level that is not in the book, reduces to
// q_n - q_{n-1} for every touched price. The asks are symmetric.
func (e *Engine) update(book *orderbook.Book, d *event.DepthEvent, t int64) {
	if !e.prevOK {
		return
	}
	bidSide := d.Side == event.SideBuy
	prev := &e.prev[orderbook.Bids]
	if !bidSide {
		prev = &e.prev[orderbook.Asks]
	}
	var world, ml float64
	for i := 0; i < int(d.NUpdates); i++ {
		lv := &d.Updates[i]
		rk := rankAt(prev, lv.Price, bidSide)
		delta := float64(lv.Qty - prevQty(prev, lv.Price))
		if !bidSide {
			delta = -delta
		}
		world += delta
		ml += delta * levelWeight(rk)
	}
	mid := e.prevMidAfter(book)
	b := e.bucketFor(t)
	b.ofi += world
	b.mlofi += ml
	e.ofi1 += world
	e.ofi5 += world
	e.ofi30 += world
	e.ml1 += ml
	e.ml5 += ml
	if old, ok := e.midAt(t - win1s); ok && old > 0 && mid > 0 {
		ret := float64(mid-old) / float64(old) * 1e4
		b.retN++
		b.retSum += ret
		b.retSq += ret * ret
		e.volN++
		e.volSum += ret
		e.volSq += ret * ret
	}
}

// trade records one print.
func (e *Engine) trade(tr *event.TradeEvent, t int64) {
	q := float64(tr.Qty)
	isBuy := tr.Side != event.SideSell
	if !isBuy {
		q = -q
	}
	b := e.bucketFor(t)
	b.trade += q
	b.tradeCnt++
	e.tr1 += q
	e.tr5 += q
	e.cnt1++

	// Advance online Hawkes process intensity
	if e.hawkesLastT > 0 && t > e.hawkesLastT {
		dtSec := float64(t-e.hawkesLastT) / 1e9
		decay := math.Exp(-2.0 * dtSec) // 0.5s half life
		e.hawkesBuy = 0.1 + (e.hawkesBuy-0.1)*decay
		e.hawkesSell = 0.1 + (e.hawkesSell-0.1)*decay
	}
	e.hawkesLastT = t
	jump := math.Log(1.0 + math.Abs(q)/float64(event.QtyScale))
	if isBuy {
		e.hawkesBuy += 0.8 * jump
		e.hawkesSell += 0.2 * jump
	} else {
		e.hawkesSell += 0.8 * jump
		e.hawkesBuy += 0.2 * jump
	}

	// Advance Volume-Synchronized Probability of Toxicity (VPIN)
	baseQty := math.Abs(q) / float64(event.QtyScale)
	bucketVol := e.vpinVolBucket
	if bucketVol <= 0 {
		bucketVol = 2.0 // default 2 base units per volume bucket
		e.vpinVolBucket = bucketVol
	}
	rem := baseQty
	for rem > 0 {
		tot := e.vpinCurrBuy + e.vpinCurrSell
		need := bucketVol - tot
		take := math.Min(rem, need)
		if isBuy {
			e.vpinCurrBuy += take
		} else {
			e.vpinCurrSell += take
		}
		rem -= take

		if e.vpinCurrBuy+e.vpinCurrSell >= bucketVol {
			diff := math.Abs(e.vpinCurrBuy - e.vpinCurrSell)
			e.vpinDiffs[e.vpinDiffIdx] = diff
			e.vpinDiffIdx = (e.vpinDiffIdx + 1) % len(e.vpinDiffs)
			if e.vpinFilled < len(e.vpinDiffs) {
				e.vpinFilled++
			}
			e.vpinCurrBuy = 0
			e.vpinCurrSell = 0
		}
	}
}

// prevMidAfter returns the mid the book has after the current update.
func (e *Engine) prevMidAfter(book *orderbook.Book) int64 {
	bp, _ := book.Levels[orderbook.Bids].Best()
	ap, _ := book.Levels[orderbook.Asks].Best()
	return (bp + ap) / 2
}

// midAt returns the most recent mid recorded at or before ref, walking back at
// most maxMidScan buckets. It reports false when the book was idle across the
// whole scan, in which case the window has no usable reference.
func (e *Engine) midAt(ref int64) (int64, bool) {
	if !e.started {
		return 0, false
	}
	s := ref / bucketNs
	oldest := e.headSeq - int64(nBuckets) + 1
	if s > e.headSeq {
		s = e.headSeq
	}
	for i := 0; i < maxMidScan && s >= oldest && s >= 0; i++ {
		b := &e.b[s&bktMask]
		if b.start != s*bucketNs {
			break // slot was reused for another bucket sequence
		}
		if b.hasMid && b.midT <= ref {
			return b.mid, true
		}
		s--
	}
	return 0, false
}

// rankAt returns the 0-based rank the level at price occupies (or would
// occupy) on the side, 0 being the touch.
func rankAt(side *orderbook.Levels, price int64, bid bool) int {
	for j := 0; j < int(side.N); j++ {
		if (bid && side.P[j] <= price) || (!bid && side.P[j] >= price) {
			return j
		}
	}
	return int(side.N)
}

func prevQty(lv *orderbook.Levels, price int64) int64 {
	for j := 0; j < int(lv.N); j++ {
		if lv.P[j] == price {
			return lv.Q[j]
		}
	}
	return 0
}

// Extract writes the NCore raw features for the current book state into dst
// and returns how many were written; it returns 0 until the first event was
// processed or if dst is too small. It never allocates.
//
// Features are emitted at event instants (from Advance), so replay and live
// produce identical values for the same event stream regardless of how often
// the consumer reads them.
func (e *Engine) Extract(book *orderbook.Book, dst []float32) int {
	if len(dst) < NCore || !e.emitted || !book.Ready {
		return 0
	}
	bids := &book.Levels[orderbook.Bids]
	asks := &book.Levels[orderbook.Asks]
	dst[0] = imbalance(bids.Q[0], asks.Q[0])
	dst[1] = imbalance(book.DepthSum(orderbook.Bids, 5), book.DepthSum(orderbook.Asks, 5))
	dst[2] = imbalance(book.DepthSum(orderbook.Bids, event.MaxLevels),
		book.DepthSum(orderbook.Asks, event.MaxLevels))
	copy(dst[3:NCore], e.emit[3:NCore])
	return NCore
}

// compute fills e.emit from the current book state and the windows at t.
func (e *Engine) compute(book *orderbook.Book, t int64) {
	out := &e.emit
	e.emitted = true
	if !book.Ready || !book.Anchored {
		for i := range out {
			out[i] = 0
		}
		return
	}
	bids := &book.Levels[orderbook.Bids]
	asks := &book.Levels[orderbook.Asks]
	bp, bq := bids.P[0], bids.Q[0]
	ap, aq := asks.P[0], asks.Q[0]
	mid := (bp + ap) / 2
	if mid > 0 && ap > bp {
		out[3] = float32((microPrice(bp, bq, ap, aq) - float64(mid)) / float64(ap-bp))
		out[4] = float32(float64(ap-bp) / float64(mid) * 1e4)
	} else {
		out[3], out[4] = 0, 0
	}

	q := float64(event.QtyScale)
	out[5] = float32(e.ofi1 / q)
	out[6] = float32(e.ofi5 / q)
	out[7] = float32(e.ofi30 / q)
	out[8] = float32(e.ml1 / q)
	out[9] = float32(e.ml5 / q)

	out[10] = float32(e.volatility())
	out[11] = float32(e.retBps(t, win1s, mid))
	out[12] = float32(e.retBps(t, win5s, mid))

	out[13] = float32(e.tr5 / q)
	out[14] = float32(e.cnt1)
	out[15] = imbalance(bq, aq)

	b := e.bucketFor(t)
	b.mid, b.midT, b.hasMid = mid, t, true
}

// levelWeight is the multi-level OFI decay for an update that lands at rank
// rk at the touch (rk is 0 for the best level). Rank 20 and deeper are capped.
func levelWeight(rk int) float64 {
	if rk > event.MaxLevels-1 {
		rk = event.MaxLevels - 1
	}
	return levelDecay[rk]
}

var levelDecay = func() (w [event.MaxLevels]float64) {
	for i := range w {
		w[i] = math.Exp(-ofiBeta * float64(i))
	}
	return w
}()

func imbalance(b, a int64) float32 {
	s := b + a
	if s <= 0 {
		return 0
	}
	return float32(float64(b-a) / float64(s))
}

// HawkesImbalance returns the current Hawkes trade arrival intensity imbalance in [-1, 1].
func (e *Engine) HawkesImbalance(t int64) float64 {
	hBuy, hSell := e.hawkesBuy, e.hawkesSell
	if e.hawkesLastT > 0 && t > e.hawkesLastT {
		dtSec := float64(t-e.hawkesLastT) / 1e9
		decay := math.Exp(-2.0 * dtSec)
		hBuy = 0.1 + (hBuy-0.1)*decay
		hSell = 0.1 + (hSell-0.1)*decay
	}
	tot := hBuy + hSell
	if tot < 1e-9 {
		return 0
	}
	return (hBuy - hSell) / tot
}

// VPIN returns the current Volume-Synchronized Probability of Toxicity in [0, 1].
func (e *Engine) VPIN() float64 {
	if e.vpinFilled == 0 {
		return 0
	}
	var sumDiff float64
	for i := 0; i < e.vpinFilled; i++ {
		sumDiff += e.vpinDiffs[i]
	}
	tot := float64(e.vpinFilled) * e.vpinVolBucket
	if tot <= 0 {
		return 0
	}
	return sumDiff / tot
}

// StoikovPrice returns the non-linear transition fair value.
func (e *Engine) StoikovPrice(book *orderbook.Book) float64 {
	if !book.Ready || !book.Anchored {
		return 0
	}
	bids := &book.Levels[orderbook.Bids]
	asks := &book.Levels[orderbook.Asks]
	bp, bq := bids.P[0], bids.Q[0]
	ap, aq := asks.P[0], asks.Q[0]
	mid := float64(bp+ap) / 2.0
	spread := float64(ap - bp)
	totQ := float64(bq + aq)
	if totQ <= 0 || spread <= 0 {
		return mid
	}
	imb := (float64(bq) - float64(aq)) / totQ
	return mid + spread*0.5*math.Tanh(1.5*imb)
}

func microPrice(bp, bq, ap, aq int64) float64 {
	if bq+aq <= 0 {
		return float64((bp + ap) / 2)
	}
	return (float64(ap)*float64(bq) + float64(bp)*float64(aq)) / float64(bq+aq)
}

func (e *Engine) retBps(t, horizon, mid int64) float64 {
	old, ok := e.midAt(t - horizon)
	if !ok || old <= 0 || mid <= 0 {
		return 0
	}
	return float64(mid-old) / float64(old) * 1e4
}

// volatility returns the sample standard deviation of the 1 s mid returns
// inside the 30 s window, in bps.
func (e *Engine) volatility() float64 {
	if e.volN < 2 {
		return 0
	}
	n := float64(e.volN)
	m2 := e.volSq - e.volSum*e.volSum/n
	if m2 <= 0 {
		return 0
	}
	return math.Sqrt(m2 / (n - 1))
}
