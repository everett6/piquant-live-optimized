package marketdata

// BookDepth is the fixed number of top price levels maintained per side.
const BookDepth = 20

// PriceLevel represents a single price level in the L2 order book.
type PriceLevel struct {
	Price float64 `json:"price"`
	Size  float64 `json:"size"`
}

// OrderBookDelta represents an incremental L2 order book delta event
// formatted following the Binance/institutional standard:
//   - U: First update ID in event
//   - u: Final update ID in event
//   - pu: Previous final update ID (must match lastUpdateID of continuous book)
//   - Bids: Updated bid price levels (size 0 deletes the level)
//   - Asks: Updated ask price levels (size 0 deletes the level)
type OrderBookDelta struct {
	U    uint64       // firstUpdateID
	u    uint64       // finalUpdateID
	pu   uint64       // prevFinalUpdateID
	Bids []PriceLevel // Updated bids
	Asks []PriceLevel // Updated asks
}

// L2OrderBook maintains an institutional sorted in-memory top-20 L2 order book
// with continuous sequence gap and out-of-order frame detection.
//
// All updates on the hot path execute in 0 B/op and 0 allocs/op.
type L2OrderBook struct {
	Symbol       string
	LastUpdateID uint64
	Initialized  bool
	NeedsSync    bool

	// Fixed top-20 level arrays: Bids sorted descending, Asks sorted ascending
	Bids    [BookDepth]PriceLevel
	NumBids int
	Asks    [BookDepth]PriceLevel
	NumAsks int

	// Gap metrics
	GapCount uint64
	Updates  uint64
}

// NewL2OrderBook creates a new, uninitialized L2OrderBook.
func NewL2OrderBook(symbol string) *L2OrderBook {
	return &L2OrderBook{
		Symbol:    symbol,
		NeedsSync: true,
	}
}

// NeedsResync reports whether a sequence gap or state violation has occurred,
// requiring a fresh snapshot fetch from the exchange.
func (b *L2OrderBook) NeedsResync() bool {
	return b.NeedsSync || !b.Initialized
}

// SetSnapshot initializes or resets the order book state from an exchange snapshot.
// Expects bids sorted descending and asks sorted ascending.
func (b *L2OrderBook) SetSnapshot(lastUpdateID uint64, bids, asks []PriceLevel) {
	b.LastUpdateID = lastUpdateID
	b.NeedsSync = false
	b.Initialized = true

	// Copy top BookDepth bids
	b.NumBids = 0
	for i := 0; i < len(bids) && i < BookDepth; i++ {
		if bids[i].Price > 0 && bids[i].Size > 0 {
			b.Bids[b.NumBids] = bids[i]
			b.NumBids++
		}
	}

	// Copy top BookDepth asks
	b.NumAsks = 0
	for i := 0; i < len(asks) && i < BookDepth; i++ {
		if asks[i].Price > 0 && asks[i].Size > 0 {
			b.Asks[b.NumAsks] = asks[i]
			b.NumAsks++
		}
	}
}

// ApplyDelta applies an incremental OrderBookDelta event to the sorted book.
//
// Sequence validation rules:
//  1. If not initialized, delta cannot be applied; NeedsResync remains true.
//  2. Continuous sequencing requires delta.pu == b.LastUpdateID.
//     If delta.pu != b.LastUpdateID:
//     - If delta.u <= b.LastUpdateID: stale/duplicate frame, safely ignored (returns true).
//     - If delta.pu > b.LastUpdateID or gap occurs: sequence gap detected!
//     Sets NeedsSync = true and returns false.
//  3. First update ID must be <= final update ID (U <= u).
//
// Operates with 0 B/op and 0 allocs/op.
func (b *L2OrderBook) ApplyDelta(delta OrderBookDelta) bool {
	if !b.Initialized {
		b.NeedsSync = true
		return false
	}

	// Stale / old delta already covered by current book state
	if delta.u <= b.LastUpdateID {
		return true
	}

	// Validate U <= u
	if delta.U > delta.u {
		b.flagGap()
		return false
	}

	// Strict sequence continuity check:
	// The incoming previous final update ID must match our book's last update ID.
	if delta.pu != b.LastUpdateID {
		b.flagGap()
		return false
	}

	// Apply bid updates (descending order)
	for i := 0; i < len(delta.Bids); i++ {
		b.applyLevel(&b.Bids, &b.NumBids, delta.Bids[i], true)
	}

	// Apply ask updates (ascending order)
	for i := 0; i < len(delta.Asks); i++ {
		b.applyLevel(&b.Asks, &b.NumAsks, delta.Asks[i], false)
	}

	// Cross-check: best bid cannot cross or equal best ask if both sides exist
	if b.NumBids > 0 && b.NumAsks > 0 && b.Bids[0].Price >= b.Asks[0].Price {
		b.flagGap()
		return false
	}

	b.LastUpdateID = delta.u
	b.Updates++
	return true
}

func (b *L2OrderBook) flagGap() {
	b.NeedsSync = true
	b.GapCount++
}

// applyLevel inserts, updates, or deletes a price level in the sorted fixed-size array.
// descending: true for bids (highest price at index 0), false for asks (lowest price at index 0).
func (b *L2OrderBook) applyLevel(levels *[BookDepth]PriceLevel, count *int, update PriceLevel, descending bool) {
	if update.Price <= 0 {
		return
	}

	n := *count
	idx := -1
	for i := 0; i < n; i++ {
		if levels[i].Price == update.Price {
			idx = i
			break
		}
	}

	// Delete level if size is 0
	if update.Size <= 0 {
		if idx >= 0 {
			copy(levels[idx:], levels[idx+1:n])
			levels[n-1] = PriceLevel{}
			*count = n - 1
		}
		return
	}

	// Update existing level
	if idx >= 0 {
		levels[idx].Size = update.Size
		return
	}

	// Level does not exist: find insertion rank
	pos := n
	for i := 0; i < n; i++ {
		if (descending && update.Price > levels[i].Price) || (!descending && update.Price < levels[i].Price) {
			pos = i
			break
		}
	}

	// If pos is beyond BookDepth, update is deeper than maintained book depth
	if pos >= BookDepth {
		return
	}

	// Shift elements to make room
	lim := n
	if lim < BookDepth {
		*count = lim + 1
	} else {
		lim = BookDepth - 1 // Evict the worst level
	}

	copy(levels[pos+1:lim+1], levels[pos:lim])
	levels[pos] = update
}

// BestBid returns the best bid price and size.
func (b *L2OrderBook) BestBid() (price, size float64) {
	if b.NumBids > 0 {
		return b.Bids[0].Price, b.Bids[0].Size
	}
	return 0, 0
}

// BestAsk returns the best ask price and size.
func (b *L2OrderBook) BestAsk() (price, size float64) {
	if b.NumAsks > 0 {
		return b.Asks[0].Price, b.Asks[0].Size
	}
	return 0, 0
}

// MidPrice returns the mid price of the book. Returns (0, false) if incomplete.
func (b *L2OrderBook) MidPrice() (float64, bool) {
	if b.NumBids > 0 && b.NumAsks > 0 {
		return (b.Bids[0].Price + b.Asks[0].Price) * 0.5, true
	}
	return 0, false
}

// Spread returns best ask minus best bid. Returns 0 if empty.
func (b *L2OrderBook) Spread() float64 {
	if b.NumBids > 0 && b.NumAsks > 0 {
		return b.Asks[0].Price - b.Bids[0].Price
	}
	return 0
}
