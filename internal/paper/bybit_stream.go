package paper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/marketdata"
)

// Flag for MarketEvent originating from Bybit
const VenueBybit uint32 = 1 << 2

// BybitStreamConfig configures the public Bybit V5 market stream ingestor.
type BybitStreamConfig struct {
	// Endpoint is the Bybit V5 WebSocket URL.
	// Default: wss://stream.bybit.com/v5/public/linear (or wss://stream.bybit.com/v5/public/spot)
	Endpoint string

	// Symbols to track (default: BTCUSDT, ETHUSDT)
	Symbols []string

	// ReconnectBase is the initial backoff duration (default: 500ms)
	ReconnectBase time.Duration

	// ReconnectMax is the max exponential backoff (default: 15s)
	ReconnectMax time.Duration

	// PingInterval is the keepalive ping interval (default: 20s)
	PingInterval time.Duration
}

// DefaultBybitStreamConfig returns institutional defaults for Bybit V5 public feeds.
func DefaultBybitStreamConfig() BybitStreamConfig {
	return BybitStreamConfig{
		Endpoint:      "wss://stream.bybit.com/v5/public/linear",
		Symbols:       []string{"BTCUSDT", "ETHUSDT"},
		ReconnectBase: 500 * time.Millisecond,
		ReconnectMax:  15 * time.Second,
		PingInterval:  20 * time.Second,
	}
}

// BybitStreamIngestor connects to Bybit's public WebSocket V5 without credentials,
// subscribes to orderbook.50 and publicTrade for linear/spot markets,
// parses frames using zero-allocation byte parsers, and pushes MarketEvents
// to an engine.SPSCRing.
type BybitStreamIngestor struct {
	cfg       BybitStreamConfig
	ring      *engine.SPSCRing
	booksMu   sync.RWMutex
	books     map[string]*marketdata.L2OrderBook
	symbolIDs map[string]uint16

	connected atomic.Bool
	stats     StreamStats

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewBybitStreamIngestor instantiates the Bybit V5 stream ingestor.
func NewBybitStreamIngestor(cfg BybitStreamConfig, ring *engine.SPSCRing) *BybitStreamIngestor {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultBybitStreamConfig().Endpoint
	}
	if len(cfg.Symbols) == 0 {
		cfg.Symbols = []string{"BTCUSDT", "ETHUSDT"}
	}
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = 500 * time.Millisecond
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = 15 * time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 20 * time.Second
	}

	books := make(map[string]*marketdata.L2OrderBook, len(cfg.Symbols))
	symIDs := make(map[string]uint16, len(cfg.Symbols))
	for idx, s := range cfg.Symbols {
		upper := strings.ToUpper(s)
		books[upper] = marketdata.NewL2OrderBook(upper)
		symIDs[upper] = uint16(idx)
	}

	return &BybitStreamIngestor{
		cfg:       cfg,
		ring:      ring,
		books:     books,
		symbolIDs: symIDs,
	}
}

// Start begins the Bybit WebSocket streaming loop.
func (b *BybitStreamIngestor) Start(ctx context.Context) {
	streamCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel

	b.wg.Add(1)
	go b.runLoop(streamCtx)
}

// Stop terminates the ingestor and waits for background routines to exit.
func (b *BybitStreamIngestor) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
}

// IsConnected reports whether Bybit WebSocket frames are currently arriving.
func (b *BybitStreamIngestor) IsConnected() bool {
	return b.connected.Load()
}

// Stats returns a snapshot of Bybit stream metrics.
func (b *BybitStreamIngestor) Stats() (total, depths, trades, gaps, reconnects, fallbacks uint64, lastNs int64) {
	return b.stats.TotalEvents.Load(),
		b.stats.DepthUpdates.Load(),
		b.stats.Trades.Load(),
		b.stats.GapsDetected.Load(),
		b.stats.Reconnects.Load(),
		b.stats.FallbackTicks.Load(),
		b.stats.LastEventNs.Load()
}

// GetL2Book returns a clone of the in-memory Bybit L2 book for a symbol.
func (b *BybitStreamIngestor) GetL2Book(symbol string) (*marketdata.L2OrderBook, bool) {
	b.booksMu.RLock()
	defer b.booksMu.RUnlock()
	book, ok := b.books[strings.ToUpper(symbol)]
	if !ok {
		return nil, false
	}
	cp := *book
	return &cp, true
}

func (b *BybitStreamIngestor) runLoop(ctx context.Context) {
	defer b.wg.Done()

	backoff := b.cfg.ReconnectBase
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = b.connectAndStream(ctx)
		b.connected.Store(false)
		if ctx.Err() != nil {
			return
		}

		b.stats.Reconnects.Add(1)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > b.cfg.ReconnectMax {
			backoff = b.cfg.ReconnectMax
		}
	}
}

func (b *BybitStreamIngestor) connectAndStream(ctx context.Context) error {
	u, err := url.Parse(b.cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid Bybit endpoint %s: %w", b.cfg.Endpoint, err)
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	var d net.Dialer
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	var conn net.Conn
	if u.Scheme == "wss" {
		conn, err = tls.DialWithDialer(&d, "tcp", host, &tls.Config{
			ServerName: strings.Split(u.Host, ":")[0],
		})
	} else {
		conn, err = d.DialContext(dialCtx, "tcp", host)
	}
	if err != nil {
		return fmt.Errorf("Bybit dial failed to %s: %w", host, err)
	}
	defer conn.Close()

	// WebSocket RFC 6455 handshake
	wsKey := make([]byte, 16)
	_, _ = rand.Read(wsKey)
	keyB64 := base64.StdEncoding.EncodeToString(wsKey)

	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", path, u.Host, keyB64)

	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("Bybit handshake write failed: %w", err)
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	respStatus, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("Bybit handshake read status failed: %w", err)
	}
	if !strings.Contains(respStatus, "101") {
		return fmt.Errorf("unexpected Bybit handshake response: %s", strings.TrimSpace(respStatus))
	}

	// Consume remaining HTTP headers
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("Bybit handshake read header failed: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Send Bybit V5 subscription frame
	// Subscribes to orderbook.50.<SYM> and publicTrade.<SYM>
	args := make([]string, 0, len(b.cfg.Symbols)*2)
	for _, sym := range b.cfg.Symbols {
		upper := strings.ToUpper(sym)
		args = append(args, fmt.Sprintf(`"orderbook.50.%s"`, upper), fmt.Sprintf(`"publicTrade.%s"`, upper))
	}
	subMsg := fmt.Sprintf(`{"op":"subscribe","args":[%s]}`, strings.Join(args, ","))
	if err := sendWSText(conn, subMsg); err != nil {
		return fmt.Errorf("Bybit subscription failed: %w", err)
	}

	b.connected.Store(true)

	// Keepalive ping loop
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()

	go func() {
		ticker := time.NewTicker(b.cfg.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if err := sendWSText(conn, `{"op":"ping"}`); err != nil {
					return
				}
			}
		}
	}()

	// Frame read loop
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		payload, op, err := readWSFrame(br)
		if err != nil {
			return err
		}

		if op == 0x8 { // Close frame
			return io.EOF
		}
		if op == 0x9 { // Ping frame -> send Pong
			_ = sendWSPong(conn, payload)
			continue
		}
		if op != 0x1 && op != 0x2 { // Text or binary only
			continue
		}

		b.HandleStreamPayload(payload)
	}
}

// HandleStreamPayload processes raw Bybit V5 JSON bytes and routes to SPSCRing.
// Operates with 0 B/op and 0 allocs/op.
func (b *BybitStreamIngestor) HandleStreamPayload(payload []byte) {
	nowNs := time.Now().UnixNano()
	b.stats.LastEventNs.Store(nowNs)

	// Ignore pong or subscription ack frames
	if bytes.Contains(payload, []byte(`"pong"`)) || bytes.Contains(payload, []byte(`"subscribe"`)) {
		return
	}

	if bytes.Contains(payload, []byte(`"orderbook.`)) {
		b.handleOrderbookPayload(payload, nowNs)
	} else if bytes.Contains(payload, []byte(`"publicTrade.`)) {
		b.handleTradePayload(payload, nowNs)
	}
}

func (b *BybitStreamIngestor) handleOrderbookPayload(payload []byte, nowNs int64) {
	var delta marketdata.OrderBookDelta
	var isSnapshot bool
	if !FastParseBybitOrderbook(payload, &delta, &isSnapshot) {
		return
	}

	symbol := FastExtractBybitSymbol(payload)

	b.booksMu.Lock()
	book, ok := b.books[symbol]
	if !ok {
		book = marketdata.NewL2OrderBook(symbol)
		b.books[symbol] = book
		b.symbolIDs[symbol] = uint16(len(b.symbolIDs))
	}
	symID := b.symbolIDs[symbol]

	if isSnapshot {
		book.SetSnapshot(delta.FinalUpdateID(), delta.Bids, delta.Asks)
	} else {
		// Delta update
		book.ApplyDelta(delta)
	}
	b.booksMu.Unlock()

	mid, hasMid := book.MidPrice()
	if !hasMid && len(delta.Bids) > 0 && len(delta.Asks) > 0 {
		mid = (delta.Bids[0].Price + delta.Asks[0].Price) * 0.5
	}

	var size float64
	if len(delta.Bids) > 0 {
		size += delta.Bids[0].Size
	}
	if len(delta.Asks) > 0 {
		size += delta.Asks[0].Size
	}

	kind := uint8(1) // Depth
	if isSnapshot {
		kind = 2 // Snapshot
	}

	b.ring.Push(engine.MarketEvent{
		Timestamp: nowNs,
		Sequence:  delta.FinalUpdateID(),
		Price:     mid,
		Size:      size,
		Side:      0,
		Kind:      kind,
		SymbolID:  symID,
		Flags:     VenueBybit,
	})

	b.stats.DepthUpdates.Add(1)
	b.stats.TotalEvents.Add(1)
}

func (b *BybitStreamIngestor) handleTradePayload(payload []byte, nowNs int64) {
	symbol := FastExtractBybitSymbol(payload)

	b.booksMu.RLock()
	symID, ok := b.symbolIDs[symbol]
	b.booksMu.RUnlock()
	if !ok {
		b.booksMu.Lock()
		if id, exists := b.symbolIDs[symbol]; exists {
			symID = id
		} else {
			symID = uint16(len(b.symbolIDs))
			b.symbolIDs[symbol] = symID
		}
		b.booksMu.Unlock()
	}

	FastParseBybitTrades(payload, func(price, qty float64, side uint8, tradeTimeMs int64) {
		ts := nowNs
		if tradeTimeMs > 0 {
			ts = tradeTimeMs * 1_000_000
		}

		b.ring.Push(engine.MarketEvent{
			Timestamp: ts,
			Sequence:  uint64(tradeTimeMs),
			Price:     price,
			Size:      qty,
			Side:      side,
			Kind:      0, // Trade
			SymbolID:  symID,
			Flags:     VenueBybit,
		})

		b.stats.Trades.Add(1)
		b.stats.TotalEvents.Add(1)
	})
}

// FastExtractBybitSymbol extracts the symbol from a Bybit V5 payload without allocations.
func FastExtractBybitSymbol(payload []byte) string {
	// 1. Check "s":"..." in payload data
	sKey := []byte(`"s":`)
	if idx := bytes.Index(payload, sKey); idx >= 0 {
		rest := payload[idx+len(sKey):]
		for len(rest) > 0 && rest[0] <= ' ' {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] == '"' {
			rest = rest[1:]
			if end := bytes.IndexByte(rest, '"'); end > 0 {
				return internBybitSymbol(rest[:end])
			}
		}
	}

	// 2. Check "topic":"orderbook.50.<SYM>" or "publicTrade.<SYM>"
	topKey := []byte(`"topic":`)
	if idx := bytes.Index(payload, topKey); idx >= 0 {
		rest := payload[idx+len(topKey):]
		for len(rest) > 0 && rest[0] <= ' ' {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] == '"' {
			rest = rest[1:]
			if end := bytes.IndexByte(rest, '"'); end > 0 {
				top := rest[:end]
				if dot := bytes.LastIndexByte(top, '.'); dot >= 0 && dot+1 < len(top) {
					return internBybitSymbol(top[dot+1:])
				}
			}
		}
	}

	return "BTCUSDT"
}

func internBybitSymbol(b []byte) string {
	switch string(b) {
	case "BTCUSDT", "btcusdt":
		return "BTCUSDT"
	case "ETHUSDT", "ethusdt":
		return "ETHUSDT"
	case "SOLUSDT", "solusdt":
		return "SOLUSDT"
	case "BNBUSDT", "bnbusdt":
		return "BNBUSDT"
	case "BTCUSDC", "btcusdc":
		return "BTCUSDC"
	case "ETHUSDC", "ethusdc":
		return "ETHUSDC"
	default:
		return strings.ToUpper(string(b))
	}
}

// FastParseBybitOrderbook extracts snapshot or delta levels into out without heap allocations.
// Operates in 0 B/op and 0 allocs/op.
func FastParseBybitOrderbook(payload []byte, out *marketdata.OrderBookDelta, isSnapshot *bool) bool {
	if out == nil || len(payload) == 0 {
		return false
	}

	out.SetUpdateIDs(0, 0, 0)
	out.NumBids = 0
	out.NumAsks = 0

	// Check type: "snapshot" vs "delta"
	if bytes.Contains(payload, []byte(`"type":"snapshot"`)) {
		*isSnapshot = true
	} else {
		*isSnapshot = false
	}

	// Find update id "u":
	uKey := []byte(`"u":`)
	if idx := bytes.Index(payload, uKey); idx >= 0 {
		rest := payload[idx+len(uKey):]
		for len(rest) > 0 && rest[0] <= ' ' {
			rest = rest[1:]
		}
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > 0 {
			if uVal, ok := marketdata.ParseFastUint(rest[:end]); ok {
				out.SetUpdateIDs(0, uVal, 0)
			}
		}
	}

	// Parse Bids "b":[
	bKey := []byte(`"b":[`)
	if idx := bytes.Index(payload, bKey); idx >= 0 {
		rest := payload[idx+len(bKey):]
		parseLevels(rest, &out.BidsStorage, &out.NumBids)
		out.Bids = out.BidsStorage[:out.NumBids]
	}

	// Parse Asks "a":[
	aKey := []byte(`"a":[`)
	if idx := bytes.Index(payload, aKey); idx >= 0 {
		rest := payload[idx+len(aKey):]
		parseLevels(rest, &out.AsksStorage, &out.NumAsks)
		out.Asks = out.AsksStorage[:out.NumAsks]
	}

	return out.NumBids > 0 || out.NumAsks > 0
}

// parseLevels parses ["<price>","<size>"] levels into storage without heap allocations.
func parseLevels(data []byte, storage *[marketdata.BookDepth]marketdata.PriceLevel, count *int) {
	n := len(data)
	i := 0
	*count = 0

	for i < n && *count < marketdata.BookDepth {
		for i < n && (data[i] <= ' ' || data[i] == ',') {
			i++
		}
		if i >= n || data[i] == ']' {
			break
		}

		if data[i] != '[' {
			i++
			continue
		}
		i++ // past '['

		// First element: Price
		for i < n && (data[i] <= ' ' || data[i] == '"') {
			i++
		}
		pStart := i
		for i < n && data[i] != '"' && data[i] != ',' {
			i++
		}
		pBytes := data[pStart:i]
		for i < n && data[i] != ',' {
			i++
		}
		if i < n && data[i] == ',' {
			i++
		}

		// Second element: Size
		for i < n && (data[i] <= ' ' || data[i] == '"') {
			i++
		}
		sStart := i
		for i < n && data[i] != '"' && data[i] != ']' {
			i++
		}
		sBytes := data[sStart:i]

		for i < n && data[i] != ']' {
			i++
		}
		if i < n && data[i] == ']' {
			i++
		}

		price, pOK := marketdata.ParseFastFloat(pBytes)
		size, sOK := marketdata.ParseFastFloat(sBytes)
		if pOK && sOK && price > 0 {
			storage[*count] = marketdata.PriceLevel{Price: price, Size: size}
			*count++
		}
	}
}

// FastParseBybitTrades parses the "data":[ {...}, ... ] array of trades.
// Invokes onTrade for every trade with 0 B/op and 0 allocs/op.
func FastParseBybitTrades(payload []byte, onTrade func(price, qty float64, side uint8, tradeTime int64)) {
	dataKey := []byte(`"data":[`)
	idx := bytes.Index(payload, dataKey)
	if idx < 0 {
		return
	}

	data := payload[idx+len(dataKey):]
	n := len(data)
	i := 0

	for i < n {
		for i < n && (data[i] <= ' ' || data[i] == ',') {
			i++
		}
		if i >= n || data[i] == ']' {
			break
		}

		if data[i] != '{' {
			i++
			continue
		}

		objStart := i
		depth := 1
		i++
		for i < n && depth > 0 {
			if data[i] == '{' {
				depth++
			} else if data[i] == '}' {
				depth--
			}
			i++
		}
		obj := data[objStart:i]

		var price, qty float64
		var tradeTime int64
		side := uint8(1) // Default Buy

		// Extract "p":
		if pIdx := bytes.Index(obj, []byte(`"p":`)); pIdx >= 0 {
			pRest := obj[pIdx+4:]
			for len(pRest) > 0 && (pRest[0] <= ' ' || pRest[0] == '"') {
				pRest = pRest[1:]
			}
			end := 0
			for end < len(pRest) && pRest[end] != '"' && pRest[end] != ',' && pRest[end] != '}' {
				end++
			}
			price, _ = marketdata.ParseFastFloat(pRest[:end])
		}

		// Extract "v":
		if vIdx := bytes.Index(obj, []byte(`"v":`)); vIdx >= 0 {
			vRest := obj[vIdx+4:]
			for len(vRest) > 0 && (vRest[0] <= ' ' || vRest[0] == '"') {
				vRest = vRest[1:]
			}
			end := 0
			for end < len(vRest) && vRest[end] != '"' && vRest[end] != ',' && vRest[end] != '}' {
				end++
			}
			qty, _ = marketdata.ParseFastFloat(vRest[:end])
		}

		// Extract "S": ("Buy" vs "Sell")
		if sIdx := bytes.Index(obj, []byte(`"S":`)); sIdx >= 0 {
			sRest := obj[sIdx+4:]
			for len(sRest) > 0 && (sRest[0] <= ' ' || sRest[0] == '"') {
				sRest = sRest[1:]
			}
			if len(sRest) > 0 && (sRest[0] == 'S' || sRest[0] == 's') {
				side = 2 // Sell
			} else {
				side = 1 // Buy
			}
		}

		// Extract "T": (timestamp ms)
		if tIdx := bytes.Index(obj, []byte(`"T":`)); tIdx >= 0 {
			tRest := obj[tIdx+4:]
			for len(tRest) > 0 && (tRest[0] <= ' ' || tRest[0] == '"') {
				tRest = tRest[1:]
			}
			end := 0
			for end < len(tRest) && tRest[end] >= '0' && tRest[end] <= '9' {
				end++
			}
			tradeTime, _ = marketdata.ParseFastInt(tRest[:end])
		}

		if price > 0 && qty > 0 {
			onTrade(price, qty, side, tradeTime)
		}
	}
}

// sendWSText writes a masked RFC 6455 text frame to the connection.
func sendWSText(w io.Writer, msg string) error {
	b := []byte(msg)
	n := len(b)
	var header []byte
	if n <= 125 {
		header = []byte{0x81, 0x80 | byte(n)}
	} else if n <= 65535 {
		header = []byte{0x81, 0x80 | 126, byte(n >> 8), byte(n & 0xFF)}
	} else {
		return errors.New("frame too large")
	}

	mask := []byte{0x4b, 0x9e, 0x12, 0x7a}
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = b[i] ^ mask[i%4]
	}

	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(mask); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// DualVenueIngestor coordinates both Binance and Bybit stream ingestors,
// pushing synchronized market events into an engine.SPSCRing.
type DualVenueIngestor struct {
	binance *MarketStreamIngestor
	bybit   *BybitStreamIngestor
	ring    *engine.SPSCRing
}

// NewDualVenueIngestor instantiates the coordinated dual-venue market ingestor.
func NewDualVenueIngestor(binanceCfg StreamConfig, bybitCfg BybitStreamConfig, ring *engine.SPSCRing) *DualVenueIngestor {
	return &DualVenueIngestor{
		binance: NewMarketStreamIngestor(binanceCfg, ring),
		bybit:   NewBybitStreamIngestor(bybitCfg, ring),
		ring:    ring,
	}
}

// Start starts both Binance and Bybit streaming ingestors.
func (d *DualVenueIngestor) Start(ctx context.Context) {
	d.binance.Start(ctx)
	d.bybit.Start(ctx)
}

// Stop stops both ingestors.
func (d *DualVenueIngestor) Stop() {
	d.binance.Stop()
	d.bybit.Stop()
}

// IsConnected returns whether both or either exchange feeds are currently connected.
func (d *DualVenueIngestor) IsConnected() (binanceConnected, bybitConnected bool) {
	return d.binance.IsConnected(), d.bybit.IsConnected()
}

// Binance returns the underlying Binance ingestor.
func (d *DualVenueIngestor) Binance() *MarketStreamIngestor {
	return d.binance
}

// Bybit returns the underlying Bybit ingestor.
func (d *DualVenueIngestor) Bybit() *BybitStreamIngestor {
	return d.bybit
}
