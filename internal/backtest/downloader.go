package backtest

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HistoricalBar represents an OHLCV bar with microstructural volume and trade metadata.
type HistoricalBar struct {
	Date         string  `json:"date"`
	UnixMs       int64   `json:"unix_ms"`
	Open         float64 `json:"open"`
	High         float64 `json:"high"`
	Low          float64 `json:"low"`
	Close        float64 `json:"close"`
	Volume       float64 `json:"volume"`         // Base asset volume
	QuoteVol     float64 `json:"quote_vol"`      // Quote asset volume (USD/USDT)
	Trades       int64   `json:"trades"`         // Number of executed trades/txs
	TakerBuyBase float64 `json:"taker_buy_base"` // Aggressive buyer volume
}

// ParkinsonVol computes Parkinson (1980) intraday high-low volatility:
//
//	\sigma_P = \sqrt{\frac{(\ln(H/L))^2}{4 \ln 2}}
func (b HistoricalBar) ParkinsonVol() float64 {
	if b.Low <= 0 || b.High <= b.Low {
		return 0.015 // Default 1.5% floor
	}
	ratio := math.Log(b.High / b.Low)
	return math.Sqrt((ratio * ratio) / (4.0 * math.Ln2))
}

// GarmanKlassVol computes Garman-Klass (1980) OHLC volatility:
//
//	\sigma_{GK} = \sqrt{0.5 (\ln(H/L))^2 - (2\ln 2 - 1) (\ln(C/O))^2}
func (b HistoricalBar) GarmanKlassVol() float64 {
	if b.Low <= 0 || b.High <= b.Low || b.Open <= 0 || b.Close <= 0 {
		return b.ParkinsonVol()
	}
	hl := math.Log(b.High / b.Low)
	co := math.Log(b.Close / b.Open)
	term1 := 0.5 * hl * hl
	term2 := (2.0*math.Ln2 - 1.0) * co * co
	diff := term1 - term2
	if diff < 1e-8 {
		diff = 1e-8
	}
	return math.Sqrt(diff)
}

// VPIN estimates volume-synchronized probability of toxicity from the bar.
func (b HistoricalBar) VPIN() float64 {
	if b.Volume <= 0 {
		return 0.50
	}
	if b.TakerBuyBase > 0 && b.TakerBuyBase <= b.Volume {
		takerSell := b.Volume - b.TakerBuyBase
		return math.Abs(b.TakerBuyBase-takerSell) / b.Volume
	}
	// Fallback when taker breakdown not available: use close location value (CLV)
	rng := b.High - b.Low
	if rng <= 0 {
		return 0.50
	}
	clv := ((b.Close - b.Low) - (b.High - b.Close)) / rng // in [-1, 1]
	buyFrac := 0.50 + 0.50*clv
	sellFrac := 1.0 - buyFrac
	return math.Abs(buyFrac - sellFrac)
}

// Downloader manages multi-source historical cryptocurrency data ingestion.
type Downloader struct {
	Client  *http.Client
	DataDir string
}

// NewDownloader returns a Downloader using dataDir for persistence.
func NewDownloader(dataDir string) *Downloader {
	if dataDir == "" {
		dataDir = "data/historical"
	}
	_ = os.MkdirAll(dataDir, 0755)
	return &Downloader{
		Client: &http.Client{
			Timeout: 30 * time.Second,
		},
		DataDir: dataDir,
	}
}

// CoinMetricsRow holds raw data extracted from CoinMetrics community CSV.
type CoinMetricsRow struct {
	Date     string
	PriceUSD float64
	TxCnt    int64
	SpotVol  float64
}

// FetchCoinMetrics fetches daily community data for BTC or ETH from GitHub.
func (d *Downloader) FetchCoinMetrics(asset string) ([]CoinMetricsRow, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/coinmetrics/data/master/csv/%s.csv", strings.ToLower(asset))
	resp, err := d.Client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("coinmetrics get %s: %w", asset, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coinmetrics %s status: %d", asset, resp.StatusCode)
	}

	reader := csv.NewReader(bufio.NewReader(resp.Body))
	reader.ReuseRecord = true

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading coinmetrics header: %w", err)
	}

	timeIdx := -1
	priceIdx := -1
	txIdx := -1
	volIdx := -1

	for i, col := range header {
		switch col {
		case "time":
			timeIdx = i
		case "PriceUSD":
			priceIdx = i
		case "TxCnt":
			txIdx = i
		case "volume_reported_spot_usd_1d":
			volIdx = i
		}
	}

	if timeIdx < 0 || priceIdx < 0 {
		return nil, fmt.Errorf("missing time or PriceUSD column in %s", asset)
	}

	var rows []CoinMetricsRow
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) <= priceIdx || rec[priceIdx] == "" {
			continue
		}
		p, err := strconv.ParseFloat(rec[priceIdx], 64)
		if err != nil || p <= 0 {
			continue
		}
		dateStr := rec[timeIdx]
		var txCnt int64
		if txIdx >= 0 && txIdx < len(rec) && rec[txIdx] != "" {
			txCnt, _ = strconv.ParseInt(rec[txIdx], 10, 64)
		}
		var spotVol float64
		if volIdx >= 0 && volIdx < len(rec) && rec[volIdx] != "" {
			spotVol, _ = strconv.ParseFloat(rec[volIdx], 64)
		}

		rows = append(rows, CoinMetricsRow{
			Date:     dateStr,
			PriceUSD: p,
			TxCnt:    txCnt,
			SpotVol:  spotVol,
		})
	}
	return rows, nil
}

// FetchYahooDaily fetches daily OHLCV from Yahoo Finance v8 chart API.
func (d *Downloader) FetchYahooDaily(symbol string, startTs, endTs int64) ([]HistoricalBar, error) {
	url := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d",
		symbol, startTs, endTs)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yahoo get %s: %w", symbol, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("yahoo %s status: %d", symbol, resp.StatusCode)
	}

	var payload struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Open   []*float64 `json:"open"`
						High   []*float64 `json:"high"`
						Low    []*float64 `json:"low"`
						Close  []*float64 `json:"close"`
						Volume []*float64 `json:"volume"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error any `json:"error"`
		} `json:"chart"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding yahoo json: %w", err)
	}

	if len(payload.Chart.Result) == 0 {
		return nil, fmt.Errorf("empty yahoo chart result for %s", symbol)
	}

	res := payload.Chart.Result[0]
	if len(res.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("no quote indicators for %s", symbol)
	}
	q := res.Indicators.Quote[0]

	var bars []HistoricalBar
	for i, ts := range res.Timestamp {
		if i >= len(q.Open) || i >= len(q.High) || i >= len(q.Low) || i >= len(q.Close) {
			break
		}
		if q.Open[i] == nil || q.High[i] == nil || q.Low[i] == nil || q.Close[i] == nil {
			continue
		}
		o, h, l, c := *q.Open[i], *q.High[i], *q.Low[i], *q.Close[i]
		if o <= 0 || h <= 0 || l <= 0 || c <= 0 {
			continue
		}
		var vol float64
		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = *q.Volume[i]
		}
		t := time.Unix(ts, 0).UTC()
		bars = append(bars, HistoricalBar{
			Date:     t.Format("2006-01-02"),
			UnixMs:   ts * 1000,
			Open:     o,
			High:     h,
			Low:      l,
			Close:    c,
			Volume:   vol / c, // approx base volume
			QuoteVol: vol,
			Trades:   1000,
		})
	}
	return bars, nil
}

// FetchBinanceKlines fetches all klines for symbol and interval from startMs to present.
func (d *Downloader) FetchBinanceKlines(symbol, interval string, startMs int64) ([]HistoricalBar, error) {
	var allBars []HistoricalBar
	currStart := startMs

	for {
		url := fmt.Sprintf("https://data-api.binance.vision/api/v3/klines?symbol=%s&interval=%s&startTime=%d&limit=1000",
			symbol, interval, currStart)
		resp, err := d.Client.Get(url)
		if err != nil {
			return nil, fmt.Errorf("binance klines %s: %w", symbol, err)
		}

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("binance %s status %d: %s", symbol, resp.StatusCode, string(b))
		}

		var rawKlines [][]any
		err = json.NewDecoder(resp.Body).Decode(&rawKlines)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decoding binance klines: %w", err)
		}

		if len(rawKlines) == 0 {
			break
		}

		var lastOpenTime int64
		for _, k := range rawKlines {
			if len(k) < 11 {
				continue
			}
			openTime, ok := k[0].(float64)
			if !ok {
				continue
			}
			lastOpenTime = int64(openTime)

			oStr, _ := k[1].(string)
			hStr, _ := k[2].(string)
			lStr, _ := k[3].(string)
			cStr, _ := k[4].(string)
			vStr, _ := k[5].(string)
			qvStr, _ := k[7].(string)
			tradesCount, _ := k[8].(float64)
			takerBuyBaseStr, _ := k[9].(string)

			o, _ := strconv.ParseFloat(oStr, 64)
			h, _ := strconv.ParseFloat(hStr, 64)
			l, _ := strconv.ParseFloat(lStr, 64)
			c, _ := strconv.ParseFloat(cStr, 64)
			v, _ := strconv.ParseFloat(vStr, 64)
			qv, _ := strconv.ParseFloat(qvStr, 64)
			tbBase, _ := strconv.ParseFloat(takerBuyBaseStr, 64)

			t := time.UnixMilli(lastOpenTime).UTC()
			allBars = append(allBars, HistoricalBar{
				Date:         t.Format("2006-01-02"),
				UnixMs:       lastOpenTime,
				Open:         o,
				High:         h,
				Low:          l,
				Close:        c,
				Volume:       v,
				QuoteVol:     qv,
				Trades:       int64(tradesCount),
				TakerBuyBase: tbBase,
			})
		}

		if len(rawKlines) < 1000 {
			break
		}
		// Advance next query start time
		currStart = lastOpenTime + 1
	}

	return allBars, nil
}

// DownloadFullDataset builds and persists comprehensive 2010/2015–2026 BTC and ETH datasets.
func (d *Downloader) DownloadFullDataset() error {
	btcFile := filepath.Join(d.DataDir, "btc_daily_full.csv")
	ethFile := filepath.Join(d.DataDir, "eth_daily_full.csv")

	// 1. Process BTC
	fmt.Printf("[Downloader] Building complete 16-year BTC historical dataset (2010–2026)...\n")
	btcBars, err := d.buildBTCDaily()
	if err != nil {
		return fmt.Errorf("building BTC dataset: %w", err)
	}
	if err := SaveHistoricalCSV(btcFile, btcBars); err != nil {
		return fmt.Errorf("saving BTC dataset: %w", err)
	}
	fmt.Printf("[Downloader] Saved %d BTC daily bars to %s (Date Range: %s to %s)\n",
		len(btcBars), btcFile, btcBars[0].Date, btcBars[len(btcBars)-1].Date)

	// 2. Process ETH
	fmt.Printf("[Downloader] Building complete 11-year ETH historical dataset (2015–2026)...\n")
	ethBars, err := d.buildETHDaily()
	if err != nil {
		return fmt.Errorf("building ETH dataset: %w", err)
	}
	if err := SaveHistoricalCSV(ethFile, ethBars); err != nil {
		return fmt.Errorf("saving ETH dataset: %w", err)
	}
	fmt.Printf("[Downloader] Saved %d ETH daily bars to %s (Date Range: %s to %s)\n",
		len(ethBars), ethFile, ethBars[0].Date, ethBars[len(ethBars)-1].Date)

	// 3. Download Binance 4h bars for high-resolution microstructure
	btc4hFile := filepath.Join(d.DataDir, "btc_4h_binance.csv")
	eth4hFile := filepath.Join(d.DataDir, "eth_4h_binance.csv")
	if _, err := os.Stat(btc4hFile); os.IsNotExist(err) {
		fmt.Printf("[Downloader] Fetching BTCUSDT 4h intraday bars from Binance Vision (2017–2026)...\n")
		// Start from 2017-08-17
		bars4h, err := d.FetchBinanceKlines("BTCUSDT", "4h", 1502928000000)
		if err == nil && len(bars4h) > 0 {
			_ = SaveHistoricalCSV(btc4hFile, bars4h)
			fmt.Printf("[Downloader] Saved %d BTC 4h bars\n", len(bars4h))
		}
	}
	if _, err := os.Stat(eth4hFile); os.IsNotExist(err) {
		fmt.Printf("[Downloader] Fetching ETHUSDT 4h intraday bars from Binance Vision (2017–2026)...\n")
		bars4h, err := d.FetchBinanceKlines("ETHUSDT", "4h", 1502928000000)
		if err == nil && len(bars4h) > 0 {
			_ = SaveHistoricalCSV(eth4hFile, bars4h)
			fmt.Printf("[Downloader] Saved %d ETH 4h bars\n", len(bars4h))
		}
	}

	return nil
}

func (d *Downloader) buildBTCDaily() ([]HistoricalBar, error) {
	// A. CoinMetrics pre-Binance BTC
	cmRows, err := d.FetchCoinMetrics("btc")
	if err != nil {
		return nil, err
	}

	// Index CoinMetrics by Date
	cmMap := make(map[string]CoinMetricsRow, len(cmRows))
	for _, row := range cmRows {
		cmMap[row.Date] = row
	}

	// B. Yahoo Finance BTC-USD from 2014-09-17 to 2017-08-16
	yahooBars, _ := d.FetchYahooDaily("BTC-USD", 1410912000, 1502928000)
	yahooMap := make(map[string]HistoricalBar, len(yahooBars))
	for _, b := range yahooBars {
		yahooMap[b.Date] = b
	}

	// C. Binance Vision BTCUSDT from 2017-08-17 to present
	binanceBars, err := d.FetchBinanceKlines("BTCUSDT", "1d", 1502928000000)
	if err != nil {
		return nil, fmt.Errorf("fetch binance BTC: %w", err)
	}
	binanceMap := make(map[string]HistoricalBar, len(binanceBars))
	for _, b := range binanceBars {
		binanceMap[b.Date] = b
	}

	// Stitch together sequentially from CoinMetrics earliest date
	var result []HistoricalBar
	var prevClose float64

	for _, cm := range cmRows {
		dateStr := cm.Date
		// If Binance has this date, use Binance's pristine L2/L3 aggregated bar
		if bb, ok := binanceMap[dateStr]; ok {
			result = append(result, bb)
			prevClose = bb.Close
			continue
		}

		// If Yahoo has this date (2014–2017), use Yahoo's genuine OHLCV
		if yb, ok := yahooMap[dateStr]; ok {
			result = append(result, yb)
			prevClose = yb.Close
			continue
		}

		// For early era (2010–2014) from CoinMetrics:
		closePrice := cm.PriceUSD
		if closePrice <= 0 {
			continue
		}
		openPrice := prevClose
		if openPrice <= 0 {
			openPrice = closePrice
		}
		prevClose = closePrice

		// Reconstruct realistic intra-day spread bounds from close-to-close drift
		ret := math.Abs(closePrice-openPrice) / openPrice
		margin := math.Max(0.015, 0.40*ret)
		highPrice := math.Max(openPrice, closePrice) * (1.0 + margin)
		lowPrice := math.Min(openPrice, closePrice) * (1.0 - margin)
		if lowPrice <= 0 {
			lowPrice = closePrice * 0.90
		}

		t, err := time.Parse("2006-01-02", dateStr)
		var unixMs int64
		if err == nil {
			unixMs = t.UnixMilli()
		}

		volUsd := cm.SpotVol
		if volUsd <= 0 {
			volUsd = float64(cm.TxCnt) * closePrice * 0.5
			if volUsd <= 0 {
				volUsd = 1000.0
			}
		}

		result = append(result, HistoricalBar{
			Date:         dateStr,
			UnixMs:       unixMs,
			Open:         openPrice,
			High:         highPrice,
			Low:          lowPrice,
			Close:        closePrice,
			Volume:       volUsd / closePrice,
			QuoteVol:     volUsd,
			Trades:       cm.TxCnt,
			TakerBuyBase: (volUsd / closePrice) * 0.50,
		})
	}

	// Append any Binance bars that may extend past CoinMetrics
	if len(result) > 0 {
		lastDate := result[len(result)-1].Date
		for _, bb := range binanceBars {
			if bb.Date > lastDate {
				result = append(result, bb)
			}
		}
	}

	return result, nil
}

func (d *Downloader) buildETHDaily() ([]HistoricalBar, error) {
	// A. CoinMetrics pre-Binance ETH (2015-08-08 to 2017-08-16)
	cmRows, err := d.FetchCoinMetrics("eth")
	if err != nil {
		return nil, err
	}

	// B. Binance Vision ETHUSDT from 2017-08-17 to present
	binanceBars, err := d.FetchBinanceKlines("ETHUSDT", "1d", 1502928000000)
	if err != nil {
		return nil, fmt.Errorf("fetch binance ETH: %w", err)
	}
	binanceMap := make(map[string]HistoricalBar, len(binanceBars))
	for _, b := range binanceBars {
		binanceMap[b.Date] = b
	}

	var result []HistoricalBar
	var prevClose float64

	for _, cm := range cmRows {
		dateStr := cm.Date
		if bb, ok := binanceMap[dateStr]; ok {
			result = append(result, bb)
			prevClose = bb.Close
			continue
		}

		closePrice := cm.PriceUSD
		if closePrice <= 0 {
			continue
		}
		openPrice := prevClose
		if openPrice <= 0 {
			openPrice = closePrice
		}
		prevClose = closePrice

		ret := math.Abs(closePrice-openPrice) / openPrice
		margin := math.Max(0.02, 0.45*ret)
		highPrice := math.Max(openPrice, closePrice) * (1.0 + margin)
		lowPrice := math.Min(openPrice, closePrice) * (1.0 - margin)
		if lowPrice <= 0 {
			lowPrice = closePrice * 0.90
		}

		t, err := time.Parse("2006-01-02", dateStr)
		var unixMs int64
		if err == nil {
			unixMs = t.UnixMilli()
		}

		volUsd := cm.SpotVol
		if volUsd <= 0 {
			volUsd = float64(cm.TxCnt) * closePrice * 1.5
			if volUsd <= 0 {
				volUsd = 1000.0
			}
		}

		result = append(result, HistoricalBar{
			Date:         dateStr,
			UnixMs:       unixMs,
			Open:         openPrice,
			High:         highPrice,
			Low:          lowPrice,
			Close:        closePrice,
			Volume:       volUsd / closePrice,
			QuoteVol:     volUsd,
			Trades:       cm.TxCnt,
			TakerBuyBase: (volUsd / closePrice) * 0.50,
		})
	}

	if len(result) > 0 {
		lastDate := result[len(result)-1].Date
		for _, bb := range binanceBars {
			if bb.Date > lastDate {
				result = append(result, bb)
			}
		}
	}

	return result, nil
}

// SaveHistoricalCSV serializes historical bars to a compact, standards-compliant CSV file.
func SaveHistoricalCSV(path string, bars []HistoricalBar) error {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// Header
	header := []string{"timestamp", "date", "open", "high", "low", "close", "volume", "trades", "quote_volume", "taker_buy_volume"}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, b := range bars {
		row := []string{
			strconv.FormatInt(b.UnixMs, 10),
			b.Date,
			strconv.FormatFloat(b.Open, 'f', 4, 64),
			strconv.FormatFloat(b.High, 'f', 4, 64),
			strconv.FormatFloat(b.Low, 'f', 4, 64),
			strconv.FormatFloat(b.Close, 'f', 4, 64),
			strconv.FormatFloat(b.Volume, 'f', 4, 64),
			strconv.FormatInt(b.Trades, 10),
			strconv.FormatFloat(b.QuoteVol, 'f', 2, 64),
			strconv.FormatFloat(b.TakerBuyBase, 'f', 4, 64),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// LoadHistoricalCSV deserializes bars from CSV.
func LoadHistoricalCSV(path string) ([]HistoricalBar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	r.ReuseRecord = true

	// Header
	header, err := r.Read()
	if err != nil {
		return nil, err
	}

	colMap := make(map[string]int, len(header))
	for i, h := range header {
		colMap[strings.ToLower(strings.TrimSpace(h))] = i
	}

	getCol := func(name string) int {
		if idx, ok := colMap[name]; ok {
			return idx
		}
		return -1
	}

	tsIdx := getCol("timestamp")
	dateIdx := getCol("date")
	oIdx := getCol("open")
	hIdx := getCol("high")
	lIdx := getCol("low")
	cIdx := getCol("close")
	vIdx := getCol("volume")
	tIdx := getCol("trades")
	qvIdx := getCol("quote_volume")
	tbIdx := getCol("taker_buy_volume")

	var bars []HistoricalBar
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}

		var b HistoricalBar
		if tsIdx >= 0 && tsIdx < len(rec) {
			b.UnixMs, _ = strconv.ParseInt(rec[tsIdx], 10, 64)
		}
		if dateIdx >= 0 && dateIdx < len(rec) {
			b.Date = rec[dateIdx]
		}
		if oIdx >= 0 && oIdx < len(rec) {
			b.Open, _ = strconv.ParseFloat(rec[oIdx], 64)
		}
		if hIdx >= 0 && hIdx < len(rec) {
			b.High, _ = strconv.ParseFloat(rec[hIdx], 64)
		}
		if lIdx >= 0 && lIdx < len(rec) {
			b.Low, _ = strconv.ParseFloat(rec[lIdx], 64)
		}
		if cIdx >= 0 && cIdx < len(rec) {
			b.Close, _ = strconv.ParseFloat(rec[cIdx], 64)
		}
		if vIdx >= 0 && vIdx < len(rec) {
			b.Volume, _ = strconv.ParseFloat(rec[vIdx], 64)
		}
		if tIdx >= 0 && tIdx < len(rec) {
			b.Trades, _ = strconv.ParseInt(rec[tIdx], 10, 64)
		}
		if qvIdx >= 0 && qvIdx < len(rec) {
			b.QuoteVol, _ = strconv.ParseFloat(rec[qvIdx], 64)
		}
		if tbIdx >= 0 && tbIdx < len(rec) {
			b.TakerBuyBase, _ = strconv.ParseFloat(rec[tbIdx], 64)
		}

		bars = append(bars, b)
	}

	return bars, nil
}
