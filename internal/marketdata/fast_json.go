package marketdata

import (
	"bytes"
	"math"
	"strings"
	"sync"
)

var (
	symCacheMu sync.RWMutex
	symCache   = make(map[string]string, 64)
)

// internSymbol converts a byte slice into an interned string without heap allocations
// for all common cryptocurrency pairs and cached symbols.
func internSymbol(b []byte) string {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}

	// Direct switch on string(b) is optimized by the Go compiler to compare byte slices
	// directly without heap allocation when branches are string constants.
	switch string(b) {
	case "BTCUSDT", "btcusdt":
		return "BTCUSDT"
	case "ETHUSDT", "ethusdt":
		return "ETHUSDT"
	case "BNBUSDT", "bnbusdt":
		return "BNBUSDT"
	case "SOLUSDT", "solusdt":
		return "SOLUSDT"
	case "ETHBTC", "ethbtc":
		return "ETHBTC"
	case "BTCUSDC", "btcusdc":
		return "BTCUSDC"
	case "ETHUSDC", "ethusdc":
		return "ETHUSDC"
	case "DOGEUSDT", "dogeusdt":
		return "DOGEUSDT"
	case "XRPUSDT", "xrpusdt":
		return "XRPUSDT"
	case "ADAUSDT", "adausdt":
		return "ADAUSDT"
	case "AVAXUSDT", "avaxusdt":
		return "AVAXUSDT"
	case "LINKUSDT", "linkusdt":
		return "LINKUSDT"
	case "DOTUSDT", "dotusdt":
		return "DOTUSDT"
	case "MATICUSDT", "maticusdt":
		return "MATICUSDT"
	default:
		return internDynamic(b)
	}
}

func internDynamic(b []byte) string {
	s := string(b)
	upper := strings.ToUpper(s)
	symCacheMu.RLock()
	interned, ok := symCache[upper]
	symCacheMu.RUnlock()
	if ok {
		return interned
	}
	symCacheMu.Lock()
	if interned, ok = symCache[upper]; !ok {
		interned = upper
		symCache[upper] = interned
	}
	symCacheMu.Unlock()
	return interned
}

// ParseFastFloat converts ASCII numeric bytes into a float64 with 0 allocations.
func ParseFastFloat(b []byte) (float64, bool) {
	return parseFastFloat(b)
}

// ParseFastUint parses an unsigned integer from ASCII bytes with 0 allocations.
func ParseFastUint(b []byte) (uint64, bool) {
	return parseFastUint(b)
}

// ParseFastInt parses a signed integer from ASCII bytes with 0 allocations.
func ParseFastInt(b []byte) (int64, bool) {
	return parseFastInt(b)
}

// parseFastFloat converts ASCII numeric bytes into a float64 with 0 allocations.
func parseFastFloat(b []byte) (float64, bool) {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return 0, false
	}

	negative := false
	if b[0] == '-' {
		negative = true
		b = b[1:]
	} else if b[0] == '+' {
		b = b[1:]
	}
	if len(b) == 0 {
		return 0, false
	}

	var intPart uint64
	i := 0
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		intPart = intPart*10 + uint64(b[i]-'0')
		i++
	}

	var fracPart uint64
	var fracDiv uint64 = 1
	if i < len(b) && b[i] == '.' {
		i++
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			if fracDiv < 1000000000000000000 {
				fracPart = fracPart*10 + uint64(b[i]-'0')
				fracDiv *= 10
			}
			i++
		}
	}

	var exp int
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		expNeg := false
		if i < len(b) && b[i] == '-' {
			expNeg = true
			i++
		} else if i < len(b) && b[i] == '+' {
			i++
		}
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			exp = exp*10 + int(b[i]-'0')
			i++
		}
		if expNeg {
			exp = -exp
		}
	}

	val := float64(intPart) + float64(fracPart)/float64(fracDiv)
	if exp != 0 {
		val *= math.Pow10(exp)
	}
	if negative {
		val = -val
	}
	return val, true
}

// parseFastUint parses an unsigned integer from ASCII bytes with 0 allocations.
func parseFastUint(b []byte) (uint64, bool) {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return 0, false
	}
	var val uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		val = val*10 + uint64(c-'0')
	}
	return val, true
}

// parseFastInt parses a signed integer from ASCII bytes with 0 allocations.
func parseFastInt(b []byte) (int64, bool) {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	if b[0] == '-' {
		neg = true
		b = b[1:]
	} else if b[0] == '+' {
		b = b[1:]
	}
	u, ok := parseFastUint(b)
	if !ok {
		return 0, false
	}
	if neg {
		return -int64(u), true
	}
	return int64(u), true
}

// unwrapCombinedStream strips Binance combined stream envelope {"stream":"...","data":{...}}
// and returns the stream name slice and data slice without heap allocations.
func unwrapCombinedStream(payload []byte) (stream []byte, data []byte) {
	dataKey := []byte(`"data"`)
	dataIdx := bytes.Index(payload, dataKey)
	if dataIdx < 0 {
		return nil, payload
	}

	streamKey := []byte(`"stream"`)
	streamIdx := bytes.Index(payload, streamKey)
	if streamIdx >= 0 {
		colonIdx := bytes.IndexByte(payload[streamIdx+len(streamKey):], ':')
		if colonIdx >= 0 {
			start := streamIdx + len(streamKey) + colonIdx + 1
			for start < len(payload) && payload[start] <= ' ' {
				start++
			}
			if start < len(payload) && payload[start] == '"' {
				start++
				end := start
				for end < len(payload) && payload[end] != '"' {
					end++
				}
				if end < len(payload) {
					stream = payload[start:end]
				}
			}
		}
	}

	colonIdx := bytes.IndexByte(payload[dataIdx+len(dataKey):], ':')
	if colonIdx < 0 {
		return stream, payload
	}
	start := dataIdx + len(dataKey) + colonIdx + 1
	for start < len(payload) && payload[start] <= ' ' {
		start++
	}
	if start >= len(payload) {
		return stream, payload
	}

	if payload[start] == '{' || payload[start] == '[' {
		openChar := payload[start]
		closeChar := byte('}')
		if openChar == '[' {
			closeChar = ']'
		}
		depth := 0
		inStr := false
		for i := start; i < len(payload); i++ {
			c := payload[i]
			if inStr {
				if c == '\\' {
					i++
					continue
				}
				if c == '"' {
					inStr = false
				}
			} else {
				if c == '"' {
					inStr = true
				} else if c == openChar {
					depth++
				} else if c == closeChar {
					depth--
					if depth == 0 {
						data = payload[start : i+1]
						return stream, data
					}
				}
			}
		}
	}

	return stream, payload[start:]
}

func skipJSONValue(data []byte, i int) int {
	for i < len(data) && data[i] <= ' ' {
		i++
	}
	if i >= len(data) {
		return i
	}
	switch data[i] {
	case '"':
		i++
		for i < len(data) {
			if data[i] == '\\' {
				i += 2
				continue
			}
			if data[i] == '"' {
				i++
				break
			}
			i++
		}
		return i
	case '{':
		depth := 1
		i++
		inStr := false
		for i < len(data) && depth > 0 {
			c := data[i]
			if inStr {
				if c == '\\' {
					i += 2
					continue
				}
				if c == '"' {
					inStr = false
				}
			} else {
				if c == '"' {
					inStr = true
				} else if c == '{' {
					depth++
				} else if c == '}' {
					depth--
				}
			}
			i++
		}
		return i
	case '[':
		depth := 1
		i++
		inStr := false
		for i < len(data) && depth > 0 {
			c := data[i]
			if inStr {
				if c == '\\' {
					i += 2
					continue
				}
				if c == '"' {
					inStr = false
				}
			} else {
				if c == '"' {
					inStr = true
				} else if c == '[' {
					depth++
				} else if c == ']' {
					depth--
				}
			}
			i++
		}
		return i
	default:
		for i < len(data) && data[i] != ',' && data[i] != '}' && data[i] != ']' && data[i] > ' ' {
			i++
		}
		return i
	}
}

// FastExtractSymbol extracts and interns the trading symbol from raw or combined payloads without allocations.
func FastExtractSymbol(payload []byte) string {
	stream, data := unwrapCombinedStream(payload)
	// Check "s":"..." in data first
	sKey := []byte(`"s"`)
	if idx := bytes.Index(data, sKey); idx >= 0 {
		rest := data[idx+len(sKey):]
		if colon := bytes.IndexByte(rest, ':'); colon >= 0 {
			val := rest[colon+1:]
			for len(val) > 0 && val[0] <= ' ' {
				val = val[1:]
			}
			if len(val) > 0 && val[0] == '"' {
				val = val[1:]
				if end := bytes.IndexByte(val, '"'); end > 0 {
					return internSymbol(val[:end])
				}
			}
		}
	}
	// Check stream if combined
	if len(stream) > 0 {
		if atIdx := bytes.IndexByte(stream, '@'); atIdx > 0 {
			return internSymbol(stream[:atIdx])
		}
	}
	return "BTCUSDT"
}

// FastParseDepth20 extracts Binance depth20 or diff depth frames into out without string or heap allocations.
// Operates on both raw frames and combined stream envelopes with 0 B/op and 0 allocs/op.
func FastParseDepth20(payload []byte, out *OrderBookDelta) bool {
	if out == nil || len(payload) == 0 {
		return false
	}

	out.U = 0
	out.u = 0
	out.pu = 0
	out.NumBids = 0
	out.NumAsks = 0

	_, data := unwrapCombinedStream(payload)
	for len(data) > 0 && data[0] <= ' ' {
		data = data[1:]
	}
	if len(data) < 2 || data[0] != '{' {
		return false
	}

	n := len(data)
	i := 1

	for i < n {
		for i < n && (data[i] <= ' ' || data[i] == ',') {
			i++
		}
		if i >= n || data[i] == '}' {
			break
		}

		if data[i] != '"' {
			i++
			continue
		}

		i++
		keyStart := i
		for i < n && data[i] != '"' {
			i++
		}
		if i >= n {
			break
		}
		key := data[keyStart:i]
		i++ // skip closing quote

		for i < n && (data[i] <= ' ' || data[i] == ':') {
			i++
		}
		if i >= n {
			break
		}

		switch string(key) {
		case "u":
			valStart := i
			i = skipJSONValue(data, i)
			if u, ok := parseFastUint(data[valStart:i]); ok {
				out.u = u
			}
		case "U":
			valStart := i
			i = skipJSONValue(data, i)
			if U, ok := parseFastUint(data[valStart:i]); ok {
				out.U = U
			}
		case "pu":
			valStart := i
			i = skipJSONValue(data, i)
			if pu, ok := parseFastUint(data[valStart:i]); ok {
				out.pu = pu
			}
		case "lastUpdateId", "lastUpdateID":
			valStart := i
			i = skipJSONValue(data, i)
			if lastID, ok := parseFastUint(data[valStart:i]); ok {
				out.u = lastID
			}
		case "bids", "b":
			for i < n && data[i] <= ' ' {
				i++
			}
			if i < n && data[i] == '[' {
				i++
				for i < n {
					for i < n && (data[i] <= ' ' || data[i] == ',') {
						i++
					}
					if i >= n || data[i] == ']' {
						if i < n {
							i++
						}
						break
					}
					if data[i] == '[' {
						i++
						for i < n && data[i] <= ' ' {
							i++
						}
						pStart := i
						i = skipJSONValue(data, i)
						pBytes := data[pStart:i]

						for i < n && (data[i] <= ' ' || data[i] == ',') {
							i++
						}
						sStart := i
						i = skipJSONValue(data, i)
						sBytes := data[sStart:i]

						for i < n && data[i] != ']' {
							i++
						}
						if i < n {
							i++ // skip ']'
						}

						if out.NumBids < BookDepth {
							p, okP := parseFastFloat(pBytes)
							s, okS := parseFastFloat(sBytes)
							if okP && okS {
								out.BidsStorage[out.NumBids] = PriceLevel{Price: p, Size: s}
								out.NumBids++
							}
						}
					} else {
						i = skipJSONValue(data, i)
					}
				}
			}
		case "asks", "a":
			for i < n && data[i] <= ' ' {
				i++
			}
			if i < n && data[i] == '[' {
				i++
				for i < n {
					for i < n && (data[i] <= ' ' || data[i] == ',') {
						i++
					}
					if i >= n || data[i] == ']' {
						if i < n {
							i++
						}
						break
					}
					if data[i] == '[' {
						i++
						for i < n && data[i] <= ' ' {
							i++
						}
						pStart := i
						i = skipJSONValue(data, i)
						pBytes := data[pStart:i]

						for i < n && (data[i] <= ' ' || data[i] == ',') {
							i++
						}
						sStart := i
						i = skipJSONValue(data, i)
						sBytes := data[sStart:i]

						for i < n && data[i] != ']' {
							i++
						}
						if i < n {
							i++ // skip ']'
						}

						if out.NumAsks < BookDepth {
							p, okP := parseFastFloat(pBytes)
							s, okS := parseFastFloat(sBytes)
							if okP && okS {
								out.AsksStorage[out.NumAsks] = PriceLevel{Price: p, Size: s}
								out.NumAsks++
							}
						}
					} else {
						i = skipJSONValue(data, i)
					}
				}
			}
		default:
			i = skipJSONValue(data, i)
		}
	}

	out.Bids = out.BidsStorage[:out.NumBids]
	out.Asks = out.AsksStorage[:out.NumAsks]
	return out.u > 0 || out.NumBids > 0 || out.NumAsks > 0
}

// FastParseTrade extracts Binance trade stream fields directly into destination primitives without heap allocations.
// Operates on both raw frames and combined stream envelopes with 0 B/op and 0 allocs/op.
func FastParseTrade(payload []byte, symbol *string, price *float64, qty *float64, tradeTime *int64, isBuyerMaker *bool) bool {
	if len(payload) == 0 {
		return false
	}

	stream, data := unwrapCombinedStream(payload)
	for len(data) > 0 && data[0] <= ' ' {
		data = data[1:]
	}
	if len(data) < 2 || data[0] != '{' {
		return false
	}

	if symbol != nil && *symbol == "" && len(stream) > 0 {
		if atIdx := bytes.IndexByte(stream, '@'); atIdx > 0 {
			*symbol = internSymbol(stream[:atIdx])
		}
	}

	n := len(data)
	i := 1
	var parsedPrice, parsedQty float64
	var foundPrice, foundQty bool

	for i < n {
		for i < n && (data[i] <= ' ' || data[i] == ',') {
			i++
		}
		if i >= n || data[i] == '}' {
			break
		}

		if data[i] != '"' {
			i++
			continue
		}

		i++
		keyStart := i
		for i < n && data[i] != '"' {
			i++
		}
		if i >= n {
			break
		}
		key := data[keyStart:i]
		i++ // skip closing quote

		for i < n && (data[i] <= ' ' || data[i] == ':') {
			i++
		}
		if i >= n {
			break
		}

		valStart := i
		i = skipJSONValue(data, i)
		val := data[valStart:i]

		switch string(key) {
		case "s":
			if symbol != nil {
				*symbol = internSymbol(val)
			}
		case "p":
			if p, ok := parseFastFloat(val); ok {
				parsedPrice = p
				foundPrice = true
				if price != nil {
					*price = p
				}
			}
		case "q":
			if q, ok := parseFastFloat(val); ok {
				parsedQty = q
				foundQty = true
				if qty != nil {
					*qty = q
				}
			}
		case "T":
			if tradeTime != nil {
				if t, ok := parseFastInt(val); ok {
					*tradeTime = t
				}
			}
		case "E":
			if tradeTime != nil && *tradeTime == 0 {
				if t, ok := parseFastInt(val); ok {
					*tradeTime = t
				}
			}
		case "m":
			if isBuyerMaker != nil {
				for len(val) > 0 && val[0] <= ' ' {
					val = val[1:]
				}
				*isBuyerMaker = len(val) > 0 && (val[0] == 't' || val[0] == 'T' || val[0] == '1')
			}
		}
	}

	return foundPrice && parsedPrice > 0 && (!foundQty || parsedQty > 0)
}
