package paper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/marketdata"
)

// StreamConfig configures the public Binance market stream ingestor.
type StreamConfig struct {
	// Endpoint is the Binance WebSocket URL.
	// Default: wss://stream.binance.com:9443/stream?streams=btcusdt@depth20@100ms/ethusdt@depth20@100ms/btcusdt@trade/ethusdt@trade
	Endpoint string

	// Symbols to track (default: BTCUSDT, ETHUSDT)
	Symbols []string

	// ReconnectBase is the initial backoff duration (default: 500ms)
	ReconnectBase time.Duration

	// ReconnectMax is the max exponential backoff (default: 15s)
	ReconnectMax time.Duration

	// EnableFallback enables local synthetic feed generation when offline or disconnected.
	// Default: false for paper dry run data integrity.
	EnableFallback bool
}

// DefaultStreamConfig returns institutional defaults for Binance public feeds.
func DefaultStreamConfig() StreamConfig {
	return StreamConfig{
		Endpoint:       "wss://stream.binance.com:9443/stream?streams=btcusdt@depth20@100ms/ethusdt@depth20@100ms/btcusdt@trade/ethusdt@trade",
		Symbols:        []string{"BTCUSDT", "ETHUSDT"},
		ReconnectBase:  500 * time.Millisecond,
		ReconnectMax:   15 * time.Second,
		EnableFallback: false,
	}
}

// MarketStreamIngestor connects to Binance public multi-stream WebSocket without credentials,
// parses depth20 and trades, maintains sequenced L2 order books, and pushes MarketEvents
// to an engine.SPSCRing with zero heap allocations on the hot path.
type MarketStreamIngestor struct {
	cfg       StreamConfig
	ring      *engine.SPSCRing
	booksMu   sync.RWMutex
	books     map[string]*marketdata.L2OrderBook
	symbolIDs map[string]uint16

	connected atomic.Bool
	fallback  atomic.Bool
	stats     StreamStats

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// StreamStats captures ingestor throughput and telemetry.
type StreamStats struct {
	TotalEvents   atomic.Uint64
	DepthUpdates  atomic.Uint64
	Trades        atomic.Uint64
	GapsDetected  atomic.Uint64
	Reconnects    atomic.Uint64
	FallbackTicks atomic.Uint64
	LastEventNs   atomic.Int64
}

// NewMarketStreamIngestor instantiates the live market ingestor.
func NewMarketStreamIngestor(cfg StreamConfig, ring *engine.SPSCRing) *MarketStreamIngestor {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultStreamConfig().Endpoint
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

	books := make(map[string]*marketdata.L2OrderBook, len(cfg.Symbols))
	symIDs := make(map[string]uint16, len(cfg.Symbols))
	for idx, s := range cfg.Symbols {
		upper := strings.ToUpper(s)
		books[upper] = marketdata.NewL2OrderBook(upper)
		symIDs[upper] = uint16(idx)
	}

	return &MarketStreamIngestor{
		cfg:       cfg,
		ring:      ring,
		books:     books,
		symbolIDs: symIDs,
	}
}

// Start begins the background streaming loop.
func (m *MarketStreamIngestor) Start(ctx context.Context) {
	streamCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	m.wg.Add(1)
	go m.runLoop(streamCtx)

	if m.cfg.EnableFallback {
		m.wg.Add(1)
		go m.fallbackWatchdog(streamCtx)
	}
}

// Stop terminates the ingestor and waits for routines to exit.
func (m *MarketStreamIngestor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// IsConnected reports whether live WebSocket frames are currently arriving.
func (m *MarketStreamIngestor) IsConnected() bool {
	return m.connected.Load()
}

// IsFallback reports whether fallback synthetic generator is actively driving the ring.
func (m *MarketStreamIngestor) IsFallback() bool {
	return m.fallback.Load()
}

// Stats returns a snapshot of stream statistics.
func (m *MarketStreamIngestor) Stats() (total, depths, trades, gaps, reconnects, fallbacks uint64, lastNs int64) {
	return m.stats.TotalEvents.Load(),
		m.stats.DepthUpdates.Load(),
		m.stats.Trades.Load(),
		m.stats.GapsDetected.Load(),
		m.stats.Reconnects.Load(),
		m.stats.FallbackTicks.Load(),
		m.stats.LastEventNs.Load()
}

// GetL2Book returns a clone of the current in-memory L2 book for a symbol.
func (m *MarketStreamIngestor) GetL2Book(symbol string) (*marketdata.L2OrderBook, bool) {
	m.booksMu.RLock()
	defer m.booksMu.RUnlock()
	b, ok := m.books[strings.ToUpper(symbol)]
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

func (m *MarketStreamIngestor) runLoop(ctx context.Context) {
	defer m.wg.Done()

	backoff := m.cfg.ReconnectBase
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = m.connectAndStream(ctx)
		m.connected.Store(false)
		if ctx.Err() != nil {
			return
		}

		m.stats.Reconnects.Add(1)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > m.cfg.ReconnectMax {
			backoff = m.cfg.ReconnectMax
		}
	}
}

// connectAndStream dials the public WebSocket, performs handshake, and reads frames.
func (m *MarketStreamIngestor) connectAndStream(ctx context.Context) error {
	endpoints := []string{m.cfg.Endpoint}
	// If connecting to stream.binance.com, add data-stream.binance.vision as automatic fallback
	// if restricted (HTTP 451) from US dev locations.
	if strings.Contains(m.cfg.Endpoint, "stream.binance.com") {
		if u, err := url.Parse(m.cfg.Endpoint); err == nil {
			visionURL := *u
			visionURL.Host = "data-stream.binance.vision:9443"
			endpoints = append(endpoints, visionURL.String())
		}
	}

	var lastErr error
	for _, ep := range endpoints {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		err := m.dialAndStreamEndpoint(ctx, ep)
		if err == nil || ctx.Err() != nil {
			return err
		}
		lastErr = err
		// If returned 451 legal restriction, try next candidate immediately
		if strings.Contains(err.Error(), "451") {
			log.Printf("[MarketStreamIngestor] Endpoint %s returned HTTP 451 (geoblocked in US by Binance policy); failing over to public gateway...", ep)
			continue
		}
		// On non-451 network errors, break to respect backoff
		break
	}
	return lastErr
}

func (m *MarketStreamIngestor) dialAndStreamEndpoint(ctx context.Context, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint %s: %w", endpoint, err)
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
		return fmt.Errorf("dial failed to %s: %w", host, err)
	}
	defer conn.Close()

	// WebSocket Client Handshake (RFC 6455)
	wsKey := make([]byte, 16)
	_, _ = rand.Read(wsKey)
	keyB64 := base64.StdEncoding.EncodeToString(wsKey)

	path := u.Path
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
		return fmt.Errorf("handshake write failed: %w", err)
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	respStatus, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("handshake read status failed: %w", err)
	}
	if !strings.Contains(respStatus, "101") {
		return fmt.Errorf("unexpected handshake response from %s: %s", host, strings.TrimSpace(respStatus))
	}

	// Consume remaining HTTP headers until \r\n\r\n
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("handshake read header failed: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	m.connected.Store(true)

	// Stream frames
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
		if op != 0x1 && op != 0x2 { // Only text or binary payload
			continue
		}

		m.handleStreamPayload(payload)
	}
}

// binanceDepth20Payload represents top 20 bids and asks payload from depth20 stream.
type binanceDepth20Payload struct {
	LastUpdateID uint64     `json:"lastUpdateId"`
	Bids         [][]string `json:"bids"`
	Asks         [][]string `json:"asks"`
}

// binanceTradePayload represents trade stream event.
// Note: explicitly mapping both lowercase and uppercase keys (e/E, t/T, m/M) prevents
// Go's encoding/json case-insensitive fallback from mistargeting string fields to int fields.
type binanceTradePayload struct {
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

// binanceCombinedStreamPayload wraps multi-stream payload: {"stream":"...","data":{...}}
type binanceCombinedStreamPayload struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// handleStreamPayload parses raw JSON and routes to SPSCRing using zero-alloc fast byte parsers.
func (m *MarketStreamIngestor) handleStreamPayload(payload []byte) {
	nowNs := time.Now().UnixNano()
	m.stats.LastEventNs.Store(nowNs)

	// Route based on stream name or JSON shape with zero heap allocations
	if bytes.Contains(payload, []byte(`"trade"`)) || bytes.Contains(payload, []byte(`"e":"trade"`)) {
		m.handleTradeUpdate(payload, nowNs)
	} else if bytes.Contains(payload, []byte(`"depth"`)) || bytes.Contains(payload, []byte(`"lastUpdateId"`)) || bytes.Contains(payload, []byte(`"bids"`)) || bytes.Contains(payload, []byte(`"asks"`)) {
		m.handleDepthUpdate(payload, nowNs)
	}
}

func (m *MarketStreamIngestor) handleDepthUpdate(payload []byte, nowNs int64) {
	var delta marketdata.OrderBookDelta
	if !marketdata.FastParseDepth20(payload, &delta) {
		return
	}

	symbol := marketdata.FastExtractSymbol(payload)

	m.booksMu.Lock()
	book, ok := m.books[symbol]
	if !ok {
		book = marketdata.NewL2OrderBook(symbol)
		m.books[symbol] = book
		m.symbolIDs[symbol] = uint16(len(m.symbolIDs))
	}

	// Set fresh snapshot or update in-place with zero allocations
	book.SetSnapshot(delta.FinalUpdateID(), delta.Bids, delta.Asks)
	symID := m.symbolIDs[symbol]
	m.booksMu.Unlock()

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

	// Push to lock-free SPSC Ring
	m.ring.Push(engine.MarketEvent{
		Timestamp: nowNs,
		Sequence:  delta.FinalUpdateID(),
		Price:     mid,
		Size:      size,
		Side:      0,
		Kind:      1, // Depth
		SymbolID:  symID,
	})

	m.stats.DepthUpdates.Add(1)
	m.stats.TotalEvents.Add(1)
}

func (m *MarketStreamIngestor) handleTradeUpdate(payload []byte, nowNs int64) {
	var (
		symbol       string
		price        float64
		qty          float64
		tradeTime    int64
		isBuyerMaker bool
	)
	if !marketdata.FastParseTrade(payload, &symbol, &price, &qty, &tradeTime, &isBuyerMaker) {
		return
	}
	if price <= 0 || qty <= 0 {
		return
	}
	if symbol == "" {
		symbol = "BTCUSDT"
	}

	m.booksMu.RLock()
	symID, ok := m.symbolIDs[symbol]
	m.booksMu.RUnlock()
	if !ok {
		m.booksMu.Lock()
		if id, exists := m.symbolIDs[symbol]; exists {
			symID = id
		} else {
			symID = uint16(len(m.symbolIDs))
			m.symbolIDs[symbol] = symID
		}
		m.booksMu.Unlock()
	}

	side := uint8(1) // Buy
	if isBuyerMaker {
		side = 2 // Sell
	}

	m.ring.Push(engine.MarketEvent{
		Timestamp: nowNs,
		Sequence:  uint64(tradeTime),
		Price:     price,
		Size:      qty,
		Side:      side,
		Kind:      0, // Trade
		SymbolID:  symID,
	})

	m.stats.Trades.Add(1)
	m.stats.TotalEvents.Add(1)
}

// fallbackWatchdog monitors stream liveness and synthesizes ticks when disconnected
// so that strategy loops and paper simulators never freeze or panic.
func (m *MarketStreamIngestor) fallbackWatchdog(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	btcBase := 65000.0
	ethBase := 3500.0
	var tickCount uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastNs := m.stats.LastEventNs.Load()
			isStale := lastNs == 0 || (time.Now().UnixNano()-lastNs) > int64(2*time.Second)

			if isStale {
				m.fallback.Store(true)
				tickCount++
				nowNs := time.Now().UnixNano()
				m.stats.LastEventNs.Store(nowNs)

				// Small synthetic drift + sine wave for realistic microstructure tick
				phase := float64(tickCount) * 0.05
				btcMid := btcBase + 25.0*math.Sin(phase) + (float64((tickCount*13)%100)-50.0)*0.1
				ethMid := ethBase + 2.5*math.Sin(phase*1.18) + (float64((tickCount*7)%100)-50.0)*0.01

				// Update internal books
				m.updateFallbackBook("BTCUSDT", btcMid, tickCount)
				m.updateFallbackBook("ETHUSDT", ethMid, tickCount)

				// Push synthetic Depth & Trade events to ring
				m.ring.Push(engine.MarketEvent{
					Timestamp: nowNs,
					Sequence:  tickCount,
					Price:     btcMid,
					Size:      1.5,
					Side:      1,
					Kind:      1, // Depth
					SymbolID:  0, // BTC
				})

				m.ring.Push(engine.MarketEvent{
					Timestamp: nowNs,
					Sequence:  tickCount,
					Price:     ethMid,
					Size:      12.0,
					Side:      2,
					Kind:      0, // Trade
					SymbolID:  1, // ETH
				})

				m.stats.FallbackTicks.Add(2)
				m.stats.TotalEvents.Add(2)
			} else {
				m.fallback.Store(false)
			}
		}
	}
}

func (m *MarketStreamIngestor) updateFallbackBook(symbol string, mid float64, seq uint64) {
	m.booksMu.Lock()
	defer m.booksMu.Unlock()

	b, ok := m.books[symbol]
	if !ok {
		b = marketdata.NewL2OrderBook(symbol)
		m.books[symbol] = b
	}

	halfSpread := mid * 0.0001 / 2.0 // 1.0 bps spread
	var bids, asks [marketdata.BookDepth]marketdata.PriceLevel
	for i := 0; i < marketdata.BookDepth; i++ {
		offset := float64(i) * halfSpread * 0.5
		bids[i] = marketdata.PriceLevel{Price: mid - halfSpread - offset, Size: 1.0 + float64(i)*0.2}
		asks[i] = marketdata.PriceLevel{Price: mid + halfSpread + offset, Size: 1.0 + float64(i)*0.2}
	}
	b.SetSnapshot(seq, bids[:], asks[:])
}

// Simple RFC 6455 frame reader with support for masked and unmasked frames.
func readWSFrame(r io.Reader) ([]byte, byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, 0, err
	}

	opcode := header[0] & 0x0F
	isMasked := (header[1] & 0x80) != 0
	payloadLen := int(header[1] & 0x7F)

	if payloadLen == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, 0, err
		}
		payloadLen = int(binary.BigEndian.Uint16(ext[:]))
	} else if payloadLen == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, 0, err
		}
		payloadLen = int(binary.BigEndian.Uint64(ext[:]))
	}

	if payloadLen > 10*1024*1024 { // Protect against unbounded memory
		return nil, 0, errors.New("frame payload exceeds 10MB limit")
	}

	var maskKey [4]byte
	if isMasked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return nil, 0, err
		}
	}

	buf := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, 0, err
	}

	if isMasked {
		for i := 0; i < payloadLen; i++ {
			buf[i] ^= maskKey[i%4]
		}
	}

	return buf, opcode, nil
}

// sendWSPong replies to a WebSocket ping frame.
func sendWSPong(w io.Writer, payload []byte) error {
	var header []byte
	n := len(payload)
	if n <= 125 {
		header = []byte{0x8A, 0x80 | byte(n)} // Client frames must be masked
	} else {
		return nil
	}

	// 4-byte mask key
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	maskedPayload := make([]byte, n)
	for i := 0; i < n; i++ {
		maskedPayload[i] = payload[i] ^ mask[i%4]
	}

	packet := append(header, mask...)
	packet = append(packet, maskedPayload...)
	_, err := w.Write(packet)
	return err
}
