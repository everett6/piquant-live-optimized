package experiment

import (
	"context"
	"path/filepath"
	"testing"

	"piquant-live/internal/config"
	"piquant-live/internal/models/common"
)

func TestQuantumSearchOptimization(t *testing.T) {
	tmpDir := t.TempDir()
	datasetPath := filepath.Join(tmpDir, "test_quantum.jsonl")

	err := GenerateSynthetic(datasetPath, SyntheticOptions{
		Venue:      "binance",
		Instrument: "BTCUSDT",
		Records:    2000,
		StartPrice: 60000.0,
		TickSize:   0.1,
		Seed:       99,
	})
	if err != nil {
		t.Fatalf("GenerateSynthetic failed: %v", err)
	}

	cfg := &config.Config{
		Symbols: []config.Symbol{{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"}},
		Targets: map[string][]string{"BTC": {"binance:BTCUSDT"}},
	}

	ps := &ParameterSearch{
		ID:      "qsearch-test-1",
		Name:    "Quantum Search Test",
		Model:   "elastic_net",
		Dataset: datasetPath,
		Horizon: common.H30s,
		Symbol:  "BTCUSDT",
	}

	opts := QuantumSearchOptions{
		Steps:            20,
		GammaInitial:     2.0,
		TemperatureStart: 1.0,
		TemperatureMin:   0.05,
		ParsimonyWeight:  0.01,
		Seed:             42,
	}

	if err := RunQuantumSearch(context.Background(), ps, opts, cfg, nil); err != nil {
		t.Fatalf("RunQuantumSearch failed: %v", err)
	}

	if ps.State != StateCompleted {
		t.Fatalf("ps.State = %s, want %s (err: %s)", ps.State, StateCompleted, ps.Error)
	}
	if len(ps.Trials) == 0 {
		t.Fatal("no trials evaluated during quantum annealing")
	}
	if ps.BestTrial == nil {
		t.Fatal("best trial is nil")
	}
	if ps.TestMetrics == nil {
		t.Fatal("test metrics on best trial is nil")
	}
	if ps.TestMetrics.DownsideDeviation == 0 {
		t.Error("expected non-zero DownsideDeviation under market drop modeling")
	}
}
