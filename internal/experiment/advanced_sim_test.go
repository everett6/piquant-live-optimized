package experiment

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/models/common"
)

func TestBinarySampleCacheRoundTripAndInvalidation(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "test_samples.pqbin")

	samples := make([]common.Sample, 120)
	for i := 0; i < 120; i++ {
		var x [common.FeatureCount]float64
		for f := 0; f < common.FeatureCount; f++ {
			x[f] = float64(i)*0.1 + float64(f)*0.01
		}
		y := math.Sin(float64(i)*0.1) * 3.5
		cls := int64(1)
		if y < 0 {
			cls = -1
		}
		samples[i] = common.Sample{
			X:         x,
			Y:         y,
			TsNs:      int64(1700000000000000000) + int64(i)*100_000_000,
			Mid:       68000.0 + float64(i)*0.5,
			Spread:    0.8,
			Vol:       1.5,
			TargetCls: cls,
		}
	}

	mtime := time.Now().UnixNano()
	if err := SaveBinarySampleCacheWithMtime(cachePath, mtime, samples); err != nil {
		t.Fatalf("SaveBinarySampleCache failed: %v", err)
	}

	stat, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat .pqbin failed: %v", err)
	}
	expectedBytes := int64(pqbinHeaderSize + len(samples)*pqbinRecordSize)
	if stat.Size() != expectedBytes {
		t.Fatalf("unexpected .pqbin size: got %d, want %d", stat.Size(), expectedBytes)
	}

	loaded, ok := LoadBinarySampleCache(cachePath, mtime)
	if !ok || len(loaded) != len(samples) {
		t.Fatalf("LoadBinarySampleCache failed: ok=%v len=%d", ok, len(loaded))
	}
	for i := range samples {
		if loaded[i].TsNs != samples[i].TsNs || loaded[i].Y != samples[i].Y || loaded[i].X[15] != samples[i].X[15] {
			t.Fatalf("mismatch at sample %d: got %+v, want %+v", i, loaded[i], samples[i])
		}
	}

	// Newer source mtime must invalidate the cache
	if _, ok := LoadBinarySampleCache(cachePath, mtime+int64(time.Minute)); ok {
		t.Errorf("expected stale .pqbin cache to be invalidated when sourceMtime > headerMtime")
	}
}

func TestCapturePublicMarketStreamAndExtractCache(t *testing.T) {
	dir := t.TempDir()
	info, err := CapturePublicMarketStream(dir, "BTCUSDT", 2, 450)
	if err != nil {
		t.Fatalf("CapturePublicMarketStream failed: %v", err)
	}
	if info.Records != 450 || info.Size <= 0 || len(info.Checksum) != 64 {
		t.Fatalf("unexpected DatasetInfo: %+v", info)
	}
	if _, err := os.Stat(info.Path + ".sha256"); err != nil {
		t.Errorf("expected .sha256 sidecar file: %v", err)
	}

	cfg := &config.Config{
		Symbols: []config.Symbol{
			{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"},
		},
		Targets: map[string][]string{
			"BTC": {"binance:BTCUSDT"},
		},
	}
	samples, err := ExtractSamples(context.Background(), info.Path, common.H30s, cfg, nil)
	if err != nil {
		t.Fatalf("ExtractSamples on captured stream failed: %v", err)
	}
	if len(samples) < 30 {
		t.Fatalf("expected >= 30 samples, got %d", len(samples))
	}

	// Verify .pqbin file was written automatically
	pqbinPath := filepath.Join(dir, filepath.Base(info.Path)+".30000000000.pqbin")
	if _, err := os.Stat(pqbinPath); err != nil {
		t.Errorf("expected automatic .pqbin creation at %s: %v", pqbinPath, err)
	}
}

func TestSimulateLatencyAlphaDecay(t *testing.T) {
	n := 150
	samples := make([]common.Sample, n)
	preds := make([]float64, n)
	for i := 0; i < n; i++ {
		sig := math.Sin(float64(i)*0.2) * 2.5
		var x [common.FeatureCount]float64
		x[4] = 1.0  // spread_bps
		x[5] = 1.8  // ofi_1s
		x[8] = 1.4  // mlofi_1s
		x[10] = 2.2 // vol_30s_bps
		samples[i] = common.Sample{X: x, Y: sig}
		preds[i] = sig * 0.9
	}

	d5, d25, d100, hl := SimulateLatencyAlphaDecay(samples, preds, 0.1, 0.2)
	if !(d5 > d25 && d25 > d100) {
		t.Errorf("expected monotonic alpha decay across 5ms > 25ms > 100ms, got 5ms=%v 25ms=%v 100ms=%v", d5, d25, d100)
	}
	if hl < 2.0 || hl > 500.0 {
		t.Errorf("expected signal half-life in [2, 500] ms, got %v", hl)
	}
}

func TestWalkForwardValidationAndThermalGuardrail(t *testing.T) {
	samples := make([]common.Sample, 160)
	for i := 0; i < 160; i++ {
		x0 := math.Sin(float64(i) * 0.18)
		var x [common.FeatureCount]float64
		x[0] = x0
		x[1] = x0 * 0.7
		samples[i] = common.Sample{
			X: x,
			Y: 2.2*x0 + 0.1*math.Cos(float64(i)),
		}
	}

	meanIC, stability, winICs := RunWalkForwardValidation(samples, 4, func(train []common.Sample) func([]float64) float64 {
		r := common.NewRidge(1e-3)
		if err := r.Fit(train, []int{0, 1}); err != nil {
			return nil
		}
		return func(f []float64) float64 {
			var x [common.FeatureCount]float64
			copy(x[:], f)
			return r.Predict(&x)
		}
	})

	if len(winICs) != 4 {
		t.Fatalf("expected 4 walk-forward windows, got %d", len(winICs))
	}
	if meanIC < 0.75 || stability <= 1.0 {
		t.Errorf("expected high walk-forward IC and positive stability on consistent signal, got IC=%v stability=%v", meanIC, stability)
	}

	if throttle, w := ShouldThrottleForThermal(62.0); throttle || w != 4 {
		t.Errorf("expected 62C -> (false, 4), got (%v, %d)", throttle, w)
	}
	if throttle, w := ShouldThrottleForThermal(77.5); !throttle || w != 2 {
		t.Errorf("expected 77.5C -> (true, 2), got (%v, %d)", throttle, w)
	}
	if throttle, w := ShouldThrottleForThermal(83.0); !throttle || w != 1 {
		t.Errorf("expected 83.0C -> (true, 1), got (%v, %d)", throttle, w)
	}
}

func BenchmarkBinarySampleCacheLoad(b *testing.B) {
	dir := b.TempDir()
	jsonlPath := filepath.Join(dir, "bench.jsonl")
	if err := GenerateSynthetic(jsonlPath, SyntheticOptions{
		Venue:      "binance",
		Instrument: "BTCUSDT",
		Records:    1500,
		StartPrice: 68000.0,
		TickSize:   0.1,
		Seed:       99,
	}); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{
		Symbols: []config.Symbol{
			{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"},
		},
		Targets: map[string][]string{
			"BTC": {"binance:BTCUSDT"},
		},
	}
	samples, err := ExtractSamples(context.Background(), jsonlPath, common.H30s, cfg, nil)
	if err != nil {
		b.Fatal(err)
	}
	pqbinPath := jsonlPath + ".30000000000.pqbin"
	info, _ := os.Stat(jsonlPath)
	mtime := info.ModTime().UnixNano()
	if err := SaveBinarySampleCacheWithMtime(pqbinPath, mtime, samples); err != nil {
		b.Fatal(err)
	}

	b.Run("PQBinFastLoad", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			loaded, ok := LoadBinarySampleCache(pqbinPath, mtime)
			if !ok || len(loaded) == 0 {
				b.Fatal("cache load failed")
			}
		}
	})

	b.Run("JSONLFullExtract", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			samplesCacheMu.Lock()
			samplesCache = make(map[string]sampleCacheEntry)
			samplesCacheMu.Unlock()
			_ = os.Remove(pqbinPath)
			extracted, err := ExtractSamples(context.Background(), jsonlPath, common.H30s, cfg, nil)
			if err != nil || len(extracted) == 0 {
				b.Fatalf("extract failed: %v", err)
			}
		}
	})
}
