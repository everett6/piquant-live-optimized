package experiment

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"piquant-live/internal/dataset"
)

// SyntheticOptions configures dataset generation.
type SyntheticOptions struct {
	Venue      string  `json:"venue"`       // default "binance"
	Instrument string  `json:"instrument"`  // default "BTCUSDT"
	Records    int     `json:"records"`     // default 2000
	StartPrice float64 `json:"start_price"` // default 65000.0
	TickSize   float64 `json:"tick_size"`   // default 0.1
	Seed       int64   `json:"seed"`        // default 42
}

// GenerateSynthetic creates a reproducible dataset of realistic L2 depth and trade updates.
func GenerateSynthetic(path string, opts SyntheticOptions) error {
	if opts.Venue == "" {
		opts.Venue = "binance"
	}
	if opts.Instrument == "" {
		opts.Instrument = "BTCUSDT"
	}
	if opts.Records <= 0 {
		opts.Records = 2000
	}
	if opts.StartPrice <= 0 {
		opts.StartPrice = 65000.0
	}
	if opts.TickSize <= 0 {
		opts.TickSize = 0.1
	}
	if opts.Seed == 0 {
		opts.Seed = 42
	}

	w, err := dataset.Create(path)
	if err != nil {
		return err
	}
	defer w.Close()

	rng := rand.New(rand.NewSource(opts.Seed))
	startTS := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	currentTS := startTS
	mid := opts.StartPrice
	seq := uint64(1)

	// 1. Initial snapshot (20 levels each side)
	bids := make([]dataset.Level, 20)
	asks := make([]dataset.Level, 20)
	for i := 0; i < 20; i++ {
		bids[i] = dataset.Level{
			Price: math.Round((mid-float64(i+1)*opts.TickSize)/opts.TickSize) * opts.TickSize,
			Qty:   math.Round((0.5+rng.Float64()*2.0)*1000) / 1000,
		}
		asks[i] = dataset.Level{
			Price: math.Round((mid+float64(i+1)*opts.TickSize)/opts.TickSize) * opts.TickSize,
			Qty:   math.Round((0.5+rng.Float64()*2.0)*1000) / 1000,
		}
	}

	snapRec := &dataset.Record{
		Version:    dataset.Version,
		Venue:      opts.Venue,
		Instrument: opts.Instrument,
		Type:       dataset.TypeSnapshot,
		SendTS:     currentTS,
		RecvTS:     currentTS + int64(time.Millisecond),
		Seq:        seq,
		Bids:       bids,
		Asks:       asks,
	}
	if err := w.Write(snapRec); err != nil {
		return err
	}

	// 2. Stream depth updates
	for i := 1; i < opts.Records; i++ {
		seq++
		// Timestamp progression: 5 to 50 milliseconds
		stepNs := int64(rng.Intn(45)+5) * int64(time.Millisecond)
		currentTS += stepNs

		// Mean-reverting random walk for mid with realistic volatility
		shock := (rng.Float64() - 0.495) * opts.TickSize * 3.0

		// Model occasional sharp cryptocurrency flash drops (liquidation cascade)
		if i%450 == 0 && i > 150 {
			shock = -opts.TickSize * (15.0 + rng.Float64()*15.0)
		}
		// Model periodic quote currency / stablecoin basis drift (e.g. USDT discount or depeg)
		if i%350 == 0 {
			shock += (rng.Float64() - 0.5) * opts.TickSize * 4.0
		}

		mid += shock
		if math.Abs(mid-opts.StartPrice) > 100*opts.TickSize {
			mid = 0.95*mid + 0.05*opts.StartPrice
		}
		mid = math.Round(mid/opts.TickSize) * opts.TickSize

		// Periodic full book snapshot every 25 updates to maintain tight L20 structure
		if i%25 == 0 {
			bids := make([]dataset.Level, 20)
			asks := make([]dataset.Level, 20)
			for l := 0; l < 20; l++ {
				bids[l] = dataset.Level{
					Price: math.Round((mid-float64(l+1)*opts.TickSize)/opts.TickSize) * opts.TickSize,
					Qty:   math.Round((0.5+rng.Float64()*2.0)*1000) / 1000,
				}
				asks[l] = dataset.Level{
					Price: math.Round((mid+float64(l+1)*opts.TickSize)/opts.TickSize) * opts.TickSize,
					Qty:   math.Round((0.5+rng.Float64()*2.0)*1000) / 1000,
				}
			}
			snapRec := &dataset.Record{
				Version:    dataset.Version,
				Venue:      opts.Venue,
				Instrument: opts.Instrument,
				Type:       dataset.TypeSnapshot,
				SendTS:     currentTS,
				RecvTS:     currentTS + int64(time.Millisecond),
				Seq:        seq,
				Bids:       bids,
				Asks:       asks,
			}
			if err := w.Write(snapRec); err != nil {
				return err
			}
			continue
		}

		// Depth update record (1 to 2 levels safely away from touch)
		numLevels := rng.Intn(2) + 1
		isBuy := rng.Float64() < 0.5
		side := dataset.Buy
		if !isBuy {
			side = dataset.Sell
		}

		levels := make([]dataset.Level, numLevels)
		if isBuy {
			// Bids strictly below touch: mid - 2*tick, mid - 3*tick...
			for l := 0; l < numLevels; l++ {
				offset := float64(l + 2)
				p := math.Round((mid-offset*opts.TickSize)/opts.TickSize) * opts.TickSize
				q := math.Round((0.1+rng.Float64()*1.8)*1000) / 1000
				levels[l] = dataset.Level{Price: p, Qty: q}
			}
			// Sort ascending for dataset.Record validation
			for j := 0; j < len(levels)/2; j++ {
				levels[j], levels[len(levels)-1-j] = levels[len(levels)-1-j], levels[j]
			}
		} else {
			// Asks strictly above touch: mid + 2*tick, mid + 3*tick...
			for l := 0; l < numLevels; l++ {
				offset := float64(l + 2)
				p := math.Round((mid+offset*opts.TickSize)/opts.TickSize) * opts.TickSize
				q := math.Round((0.1+rng.Float64()*1.8)*1000) / 1000
				levels[l] = dataset.Level{Price: p, Qty: q}
			}
		}

		depthRec := &dataset.Record{
			Version:    dataset.Version,
			Venue:      opts.Venue,
			Instrument: opts.Instrument,
			Type:       dataset.TypeDepth,
			Side:       side,
			SendTS:     currentTS,
			RecvTS:     currentTS + int64(time.Millisecond),
			Seq:        seq,
		}
		if isBuy {
			depthRec.Bids = levels
		} else {
			depthRec.Asks = levels
		}
		if err := w.Write(depthRec); err != nil {
			return err
		}
	}

	return nil
}

// GenerateStandardDatasets populates dataDir with standard reproducible datasets
// if they do not already exist.
func GenerateStandardDatasets(dataDir string) error {
	btcPath := fmt.Sprintf("%s/btcusdt_sample.jsonl", dataDir)
	if err := GenerateSynthetic(btcPath, SyntheticOptions{
		Venue:      "binance",
		Instrument: "BTCUSDT",
		Records:    3000,
		StartPrice: 68500.0,
		TickSize:   0.1,
		Seed:       101,
	}); err != nil {
		return err
	}

	ethPath := fmt.Sprintf("%s/ethusdt_sample.jsonl", dataDir)
	return GenerateSynthetic(ethPath, SyntheticOptions{
		Venue:      "binance",
		Instrument: "ETHUSDT",
		Records:    3000,
		StartPrice: 3450.0,
		TickSize:   0.01,
		Seed:       202,
	})
}
