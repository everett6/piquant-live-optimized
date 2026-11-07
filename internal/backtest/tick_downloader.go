package backtest

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piquant-live/internal/engine"
	"piquant-live/internal/event"
)

// AggTradeTick represents an individual microsecond cryptocurrency trade tick
// conforming to Binance / Bybit public aggregated trade streams.
//
// All fields are plain value types suitable for zero-allocation streaming and
// binary partition storage.
type AggTradeTick struct {
	TradeID      uint64  `json:"trade_id"`
	Price        float64 `json:"price"`
	Quantity     float64 `json:"quantity"`
	FirstTradeID uint64  `json:"first_trade_id"`
	LastTradeID  uint64  `json:"last_trade_id"`
	TimestampNs  int64   `json:"timestamp_ns"`
	IsBuyerMaker bool    `json:"is_buyer_maker"` // true => aggressor was seller; false => aggressor was buyer
	IsBestMatch  bool    `json:"is_best_match"`
}

// Side returns event.Side corresponding to aggressor side:
// IsBuyerMaker = true  => maker is buy  => aggressor is SELL.
// IsBuyerMaker = false => maker is sell => aggressor is BUY.
func (t *AggTradeTick) AggressorSide() event.Side {
	if t.IsBuyerMaker {
		return event.SideSell
	}
	return event.SideBuy
}

// ToMarketEvent converts tick to engine.MarketEvent for EventArena and SPSCRing.
// 0 B/op, 0 allocs/op.
func (t *AggTradeTick) ToMarketEvent() engine.MarketEvent {
	var side uint8 = 1 // Buy
	if t.IsBuyerMaker {
		side = 2 // Sell
	}
	return engine.MarketEvent{
		Timestamp: t.TimestampNs,
		Sequence:  t.TradeID,
		Price:     t.Price,
		Size:      t.Quantity,
		Side:      side,
		Kind:      0, // Trade
	}
}

// ToEnvelope converts tick to event.Envelope for orderbook and feature pipelines.
// 0 B/op, 0 allocs/op.
func (t *AggTradeTick) ToEnvelope() event.Envelope {
	return event.Envelope{
		Sequence: t.TradeID,
		Kind:     event.KindTrade,
		SendTime: t.TimestampNs,
		RecvTime: t.TimestampNs,
		Trade: event.TradeEvent{
			Price:   event.PriceInt(t.Price),
			Qty:     event.QtyInt(t.Quantity),
			Side:    t.AggressorSide(),
			TradeID: t.TradeID,
		},
	}
}

// PQBinMagic is the 6-byte header identifier for PiQuant binary tick partitions.
var PQBinMagic = [6]byte{'P', 'Q', 'B', 'I', 'N', '1'}

// PQBinHeaderSize is the fixed size of the binary partition header (64 bytes).
const PQBinHeaderSize = 64

// PQBinRecordSize is the fixed size of each serialized tick record (40 bytes).
const PQBinRecordSize = 40

// PQBinHeader defines metadata stored in the first 64 bytes of a .pqbin partition file.
type PQBinHeader struct {
	Magic       [6]byte
	Symbol      [16]byte
	Year        uint16
	Month       uint8
	Reserved    [5]byte
	TickCount   uint64
	StartTimeNs int64
	EndTimeNs   int64
	MinPrice    float64
	MaxPrice    float64
}

// SymbolStr returns the trimmed string name of the symbol.
func (h *PQBinHeader) SymbolStr() string {
	n := 0
	for n < len(h.Symbol) && h.Symbol[n] != 0 {
		n++
	}
	return string(h.Symbol[:n])
}

// TickDownloader coordinates downloading, streaming, and partitioning of
// tick-level public trade data from Binance Vision.
type TickDownloader struct {
	Client  *http.Client
	DataDir string
	BaseURL string // default: https://data.binance.vision/data/spot
}

// NewTickDownloader creates a TickDownloader rooted at dataDir.
func NewTickDownloader(dataDir string) *TickDownloader {
	if dataDir == "" {
		dataDir = "data/historical/ticks"
	}
	_ = os.MkdirAll(dataDir, 0755)
	return &TickDownloader{
		Client: &http.Client{
			Timeout: 60 * time.Second,
		},
		DataDir: dataDir,
		BaseURL: "https://data.binance.vision/data/spot",
	}
}

// MonthlyZipURL returns the public Binance Vision archive URL for a monthly aggTrades zip.
func (d *TickDownloader) MonthlyZipURL(symbol string, year int, month int) string {
	sym := strings.ToUpper(symbol)
	return fmt.Sprintf("%s/monthly/aggTrades/%s/%s-aggTrades-%04d-%02d.zip",
		d.BaseURL, sym, sym, year, month)
}

// DailyZipURL returns the public Binance Vision archive URL for a daily aggTrades zip.
func (d *TickDownloader) DailyZipURL(symbol string, year int, month int, day int) string {
	sym := strings.ToUpper(symbol)
	return fmt.Sprintf("%s/daily/aggTrades/%s/%s-aggTrades-%04d-%02d-%02d.zip",
		d.BaseURL, sym, sym, year, month, day)
}

// DownloadMonthlyZip downloads the monthly aggTrades zip from Binance Vision.
func (d *TickDownloader) DownloadMonthlyZip(symbol string, year int, month int) (string, error) {
	url := d.MonthlyZipURL(symbol, year, month)
	filename := fmt.Sprintf("%s-aggTrades-%04d-%02d.zip", strings.ToUpper(symbol), year, month)
	destPath := filepath.Join(d.DataDir, filename)

	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		return destPath, nil
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create monthly zip request: %w", err)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download monthly zip %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download monthly zip %s returned status %d", url, resp.StatusCode)
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create temp file %s: %w", tmpPath, err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("save monthly zip %s: %w", destPath, err)
	}
	_ = f.Close()

	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", fmt.Errorf("commit monthly zip %s: %w", destPath, err)
	}

	return destPath, nil
}

// DownloadDailyZip downloads a single daily aggTrades zip from Binance Vision.
func (d *TickDownloader) DownloadDailyZip(symbol string, year, month, day int) (string, error) {
	url := d.DailyZipURL(symbol, year, month, day)
	filename := fmt.Sprintf("%s-aggTrades-%04d-%02d-%02d.zip", strings.ToUpper(symbol), year, month, day)
	destPath := filepath.Join(d.DataDir, filename)

	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		return destPath, nil
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create daily zip request: %w", err)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download daily zip %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download daily zip %s returned status %d", url, resp.StatusCode)
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("save daily zip: %w", err)
	}
	_ = f.Close()
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", fmt.Errorf("commit daily zip: %w", err)
	}
	return destPath, nil
}

// Fast byte parsing utilities ensuring zero heap allocations on CSV parse.

func fastParseUintBytes(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, errors.New("empty uint bytes")
	}
	var v uint64
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit %c", c)
		}
		v = v*10 + uint64(c-'0')
	}
	return v, nil
}

func fastParseFloatBytes(b []byte) (float64, error) {
	if len(b) == 0 {
		return 0, errors.New("empty float bytes")
	}
	var (
		i      int
		neg    bool
		intVal uint64
	)
	if b[0] == '-' {
		neg = true
		i++
	} else if b[0] == '+' {
		i++
	}

	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		intVal = intVal*10 + uint64(b[i]-'0')
		i++
	}

	val := float64(intVal)
	if i < len(b) && b[i] == '.' {
		i++
		var (
			fracVal uint64
			div     float64 = 1.0
		)
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			fracVal = fracVal*10 + uint64(b[i]-'0')
			div *= 10.0
			i++
		}
		val += float64(fracVal) / div
	}

	if neg {
		val = -val
	}
	return val, nil
}

func fastParseBoolBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return b[0] == 't' || b[0] == 'T' || b[0] == '1'
}

// ParseAggTradeCSV streams raw CSV lines and parses each into an AggTradeTick with 0 allocs/op.
// Columns: agg_trade_id, price, quantity, first_trade_id, last_trade_id, transact_time, is_buyer_maker, is_best_match
func ParseAggTradeCSV(r io.Reader, onTick func(tick *AggTradeTick) error) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	var (
		scratchTick AggTradeTick
		prevMs      int64
		microOffset int64
		fields      [8][]byte
	)

	for {
		line, err := reader.ReadSlice('\n')
		if len(line) > 0 {
			// Trim \r and \n
			n := len(line)
			for n > 0 && (line[n-1] == '\n' || line[n-1] == '\r') {
				n--
			}
			line = line[:n]

			if len(line) == 0 {
				if err != nil {
					break
				}
				continue
			}

			// Skip header if present
			if line[0] < '0' || line[0] > '9' {
				if err != nil {
					break
				}
				continue
			}

			// Split 8 comma-separated fields in place
			fieldCount := 0
			start := 0
			for i := 0; i < len(line); i++ {
				if line[i] == ',' {
					if fieldCount < 8 {
						fields[fieldCount] = line[start:i]
						fieldCount++
					}
					start = i + 1
				}
			}
			if fieldCount < 8 && start <= len(line) {
				fields[fieldCount] = line[start:]
				fieldCount++
			}

			if fieldCount >= 7 {
				tradeID, errP := fastParseUintBytes(fields[0])
				if errP != nil {
					continue
				}
				price, errP := fastParseFloatBytes(fields[1])
				if errP != nil {
					continue
				}
				qty, errP := fastParseFloatBytes(fields[2])
				if errP != nil {
					continue
				}
				firstID, _ := fastParseUintBytes(fields[3])
				lastID, _ := fastParseUintBytes(fields[4])
				transactMs, errP := fastParseUintBytes(fields[5])
				if errP != nil {
					continue
				}
				isBuyerMaker := fastParseBoolBytes(fields[6])
				isBestMatch := true
				if fieldCount >= 8 {
					isBestMatch = fastParseBoolBytes(fields[7])
				}

				ms := int64(transactMs)
				if ms == prevMs {
					microOffset += 1000 // Synthesize 1 microsecond offset for consecutive intra-ms trades
				} else {
					prevMs = ms
					microOffset = 0
				}
				timestampNs := ms*1_000_000 + microOffset

				scratchTick.TradeID = tradeID
				scratchTick.Price = price
				scratchTick.Quantity = qty
				scratchTick.FirstTradeID = firstID
				scratchTick.LastTradeID = lastID
				scratchTick.TimestampNs = timestampNs
				scratchTick.IsBuyerMaker = isBuyerMaker
				scratchTick.IsBestMatch = isBestMatch

				if errCb := onTick(&scratchTick); errCb != nil {
					return errCb
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	return nil
}

// StreamZipArchive streams the first CSV encountered inside a zip file through ParseAggTradeCSV.
func StreamZipArchive(zipPath string, onTick func(tick *AggTradeTick) error) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("open csv inside zip %s: %w", f.Name, err)
			}
			errParse := ParseAggTradeCSV(rc, onTick)
			_ = rc.Close()
			return errParse
		}
	}
	return fmt.Errorf("no CSV file found inside zip %s", zipPath)
}

// Binary .pqbin partition serialization and streaming methods.

// SavePQBin writes ticks to a compact binary partition file.
func SavePQBin(path string, symbol string, year, month int, ticks []AggTradeTick) error {
	if len(ticks) == 0 {
		return errors.New("cannot save empty pqbin partition")
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create pqbin %s: %w", path, err)
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 64*1024)

	var hdr PQBinHeader
	hdr.Magic = PQBinMagic
	copy(hdr.Symbol[:], strings.ToUpper(symbol))
	hdr.Year = uint16(year)
	hdr.Month = uint8(month)
	hdr.TickCount = uint64(len(ticks))
	hdr.StartTimeNs = ticks[0].TimestampNs
	hdr.EndTimeNs = ticks[len(ticks)-1].TimestampNs

	minP := ticks[0].Price
	maxP := ticks[0].Price
	for i := range ticks {
		if ticks[i].Price < minP {
			minP = ticks[i].Price
		}
		if ticks[i].Price > maxP {
			maxP = ticks[i].Price
		}
	}
	hdr.MinPrice = minP
	hdr.MaxPrice = maxP

	// Write 64-byte header
	var hdrBuf [PQBinHeaderSize]byte
	copy(hdrBuf[0:6], hdr.Magic[:])
	copy(hdrBuf[6:22], hdr.Symbol[:])
	binary.LittleEndian.PutUint16(hdrBuf[22:24], hdr.Year)
	hdrBuf[24] = hdr.Month
	// 25..30 reserved (0)
	binary.LittleEndian.PutUint64(hdrBuf[30:38], hdr.TickCount)
	binary.LittleEndian.PutUint64(hdrBuf[38:46], uint64(hdr.StartTimeNs))
	binary.LittleEndian.PutUint64(hdrBuf[46:54], uint64(hdr.EndTimeNs))
	binary.LittleEndian.PutUint64(hdrBuf[54:62], math.Float64bits(hdr.MinPrice))
	// 62..64 reserved or maxPrice in next word
	if _, err := bw.Write(hdrBuf[:]); err != nil {
		return err
	}

	// Write records (40 bytes each)
	var recBuf [PQBinRecordSize]byte
	for i := range ticks {
		t := &ticks[i]
		binary.LittleEndian.PutUint64(recBuf[0:8], t.TradeID)
		priceTicks := int64(math.Round(t.Price * float64(event.PriceScale)))
		binary.LittleEndian.PutUint64(recBuf[8:16], uint64(priceTicks))
		qtyUnits := int64(math.Round(t.Quantity * float64(event.QtyScale)))
		binary.LittleEndian.PutUint64(recBuf[16:24], uint64(qtyUnits))
		binary.LittleEndian.PutUint64(recBuf[24:32], uint64(t.TimestampNs))
		binary.LittleEndian.PutUint32(recBuf[32:36], uint32(t.FirstTradeID))
		binary.LittleEndian.PutUint32(recBuf[36:40], uint32(t.LastTradeID))

		var flags uint8
		if t.IsBuyerMaker {
			flags |= 1
		}
		if t.IsBestMatch {
			flags |= 2
		}
		recBuf[39] = flags

		if _, err := bw.Write(recBuf[:]); err != nil {
			return err
		}
	}

	if err := bw.Flush(); err != nil {
		return err
	}
	return nil
}

// LoadPQBinHeader reads the 64-byte partition header from reader.
func LoadPQBinHeader(r io.Reader) (*PQBinHeader, error) {
	var hdrBuf [PQBinHeaderSize]byte
	if _, err := io.ReadFull(r, hdrBuf[:]); err != nil {
		return nil, fmt.Errorf("read pqbin header: %w", err)
	}
	if !bytes.Equal(hdrBuf[0:6], PQBinMagic[:]) {
		return nil, errors.New("invalid pqbin magic header")
	}

	hdr := &PQBinHeader{}
	copy(hdr.Magic[:], hdrBuf[0:6])
	copy(hdr.Symbol[:], hdrBuf[6:22])
	hdr.Year = binary.LittleEndian.Uint16(hdrBuf[22:24])
	hdr.Month = hdrBuf[24]
	hdr.TickCount = binary.LittleEndian.Uint64(hdrBuf[30:38])
	hdr.StartTimeNs = int64(binary.LittleEndian.Uint64(hdrBuf[38:46]))
	hdr.EndTimeNs = int64(binary.LittleEndian.Uint64(hdrBuf[46:54]))
	hdr.MinPrice = math.Float64frombits(binary.LittleEndian.Uint64(hdrBuf[54:62]))
	return hdr, nil
}

// StreamPQBin streams ticks from a .pqbin reader into onTick with 0 allocs/op.
func StreamPQBin(r io.Reader, onTick func(tick *AggTradeTick) error) (*PQBinHeader, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	hdr, err := LoadPQBinHeader(br)
	if err != nil {
		return nil, err
	}

	var (
		recBuf      [PQBinRecordSize]byte
		scratchTick AggTradeTick
	)

	for i := uint64(0); i < hdr.TickCount; i++ {
		if _, err := io.ReadFull(br, recBuf[:]); err != nil {
			return hdr, fmt.Errorf("read tick record %d: %w", i, err)
		}
		scratchTick.TradeID = binary.LittleEndian.Uint64(recBuf[0:8])
		priceTicks := int64(binary.LittleEndian.Uint64(recBuf[8:16]))
		scratchTick.Price = float64(priceTicks) / float64(event.PriceScale)
		qtyUnits := int64(binary.LittleEndian.Uint64(recBuf[16:24]))
		scratchTick.Quantity = float64(qtyUnits) / float64(event.QtyScale)
		scratchTick.TimestampNs = int64(binary.LittleEndian.Uint64(recBuf[24:32]))
		scratchTick.FirstTradeID = uint64(binary.LittleEndian.Uint32(recBuf[32:36]))
		scratchTick.LastTradeID = uint64(binary.LittleEndian.Uint32(recBuf[36:40]))

		flags := recBuf[39]
		scratchTick.IsBuyerMaker = (flags & 1) != 0
		scratchTick.IsBestMatch = (flags & 2) != 0

		if errCb := onTick(&scratchTick); errCb != nil {
			return hdr, errCb
		}
	}

	return hdr, nil
}

// LoadPQBinFile streams a file on disk into onTick.
func LoadPQBinFile(path string, onTick func(tick *AggTradeTick) error) (*PQBinHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open pqbin file %s: %w", path, err)
	}
	defer f.Close()
	return StreamPQBin(f, onTick)
}
