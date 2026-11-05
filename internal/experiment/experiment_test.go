package experiment

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"piquant-live/internal/config"
	"piquant-live/internal/models/common"
)

func TestMetricsCalculation(t *testing.T) {
	pred := []float64{1.0, 2.0, -1.0, -2.0, 0.0}
	act := []float64{1.2, 1.8, -0.8, -2.2, 0.1}

	m := ComputeMetrics(pred, act)
	if m.Samples != 5 {
		t.Fatalf("samples = %d, want 5", m.Samples)
	}
	// All directions matched
	if m.DirectionalAccuracy != 1.0 {
		t.Fatalf("directional accuracy = %f, want 1.0", m.DirectionalAccuracy)
	}
	if m.MAE <= 0 || m.MAE > 0.3 {
		t.Fatalf("MAE = %f, want ~0.2", m.MAE)
	}
	if m.RankCorrelation <= 0.90 {
		t.Fatalf("Rank correlation = %f, want > 0.90", m.RankCorrelation)
	}
	if _, ok := m.Baselines["zero_mae"]; !ok {
		t.Fatal("missing zero_mae baseline")
	}
}

func TestSplitConfigValidation(t *testing.T) {
	valid := SplitConfig{TrainPct: 0.60, ValPct: 0.20, TestPct: 0.20}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid split rejected: %v", err)
	}

	invalidSum := SplitConfig{TrainPct: 0.50, ValPct: 0.20, TestPct: 0.20}
	if err := invalidSum.Validate(); err == nil {
		t.Fatal("invalid sum split accepted")
	}

	tinyTrain := SplitConfig{TrainPct: 0.10, ValPct: 0.45, TestPct: 0.45}
	if err := tinyTrain.Validate(); err == nil {
		t.Fatal("tiny train split accepted")
	}
}

func TestGeneralizationDiagnostics(t *testing.T) {
	train := Metrics{Samples: 100, MAE: 1.0, DirectionalAccuracy: 0.65, RankCorrelation: 0.30}
	valOverfit := Metrics{Samples: 100, MAE: 2.0, DirectionalAccuracy: 0.45, RankCorrelation: -0.10}
	testOverfit := valOverfit

	diag := CheckGeneralization(train, valOverfit, testOverfit)
	if diag == "" {
		t.Fatal("expected overfit diagnostic warning, got empty string")
	}
}

func TestEndToEndExperimentAndSearch(t *testing.T) {
	tmpDir := t.TempDir()
	datasetPath := filepath.Join(tmpDir, "test_btc.jsonl")

	// 1. Generate synthetic dataset
	err := GenerateSynthetic(datasetPath, SyntheticOptions{
		Venue:      "binance",
		Instrument: "BTCUSDT",
		Records:    2500,
		StartPrice: 50000.0,
		TickSize:   0.1,
		Seed:       42,
	})
	if err != nil {
		t.Fatalf("GenerateSynthetic failed: %v", err)
	}

	cfg := &config.Config{
		Symbols: []config.Symbol{
			{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"},
		},
		Targets: map[string][]string{
			"BTC": {"binance:BTCUSDT"},
		},
	}

	mgr, err := NewManager(tmpDir, cfg)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// 2. Feasibility check
	feas := mgr.ValidateFeasibility(datasetPath, common.H30s)
	if !feas.Feasible {
		t.Fatalf("Feasibility check reported infeasible: %v", feas.Errors)
	}

	// 3. Run individual experiment
	exp := &Experiment{
		ID:         "test-exp-1",
		Name:       "Test MLOFI",
		Model:      "mlofi_ridge",
		FeatureSet: "L5",
		Dataset:    datasetPath,
		Horizon:    common.H30s,
		Symbol:     "BTCUSDT",
		Params:     map[string]any{"lambda": 0.01},
		Split:      DefaultSplit(common.H30s),
	}

	if err := Run(context.Background(), exp, cfg, nil); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if exp.State != StateCompleted {
		t.Fatalf("exp state = %s, want %s (err: %s)", exp.State, StateCompleted, exp.Error)
	}
	if exp.TrainMetrics.Samples == 0 || exp.ValMetrics.Samples == 0 || exp.TestMetrics.Samples == 0 {
		t.Fatalf("split sample counts empty: train=%d, val=%d, test=%d",
			exp.TrainMetrics.Samples, exp.ValMetrics.Samples, exp.TestMetrics.Samples)
	}
	if exp.ValMetrics.MAE <= 0 || math.IsNaN(exp.ValMetrics.MAE) {
		t.Fatalf("invalid val MAE: %f", exp.ValMetrics.MAE)
	}

	// 4. Run parameter search
	ps := &ParameterSearch{
		ID:      "test-search-1",
		Name:    "Test Grid Search",
		Model:   "mlofi_ridge",
		Dataset: datasetPath,
		Horizon: common.H30s,
		Symbol:  "BTCUSDT",
	}

	space := SearchSpace{
		FeatureSets: []string{"L1", "L5"},
		Lambdas:     []float64{0.001, 0.1},
	}

	if err := RunSearch(context.Background(), ps, space, cfg, nil); err != nil {
		t.Fatalf("RunSearch failed: %v", err)
	}

	if ps.State != StateCompleted {
		t.Fatalf("search state = %s, want %s", ps.State, StateCompleted)
	}
	if len(ps.Trials) != 4 {
		t.Fatalf("trials = %d, want 4", len(ps.Trials))
	}
	if ps.BestTrial == nil {
		t.Fatal("best trial is nil")
	}
	if ps.TestMetrics == nil {
		t.Fatal("test metrics on best trial is nil")
	}
}

func TestAllModelsAndMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	datasetPath := filepath.Join(tmpDir, "test_multi_model.jsonl")

	err := GenerateSynthetic(datasetPath, SyntheticOptions{
		Venue:      "binance",
		Instrument: "BTCUSDT",
		Records:    2500,
		StartPrice: 50000.0,
		TickSize:   0.1,
		Seed:       123,
	})
	if err != nil {
		t.Fatalf("GenerateSynthetic failed: %v", err)
	}

	cfg := &config.Config{
		Symbols: []config.Symbol{{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"}},
		Targets: map[string][]string{"BTC": {"binance:BTCUSDT"}},
	}

	modelsToTest := []string{"mlofi_ridge", "elastic_net", "adaptive_rls", "queue_logistic"}
	for _, mName := range modelsToTest {
		exp := &Experiment{
			ID:         "test-" + mName,
			Name:       "Test " + mName,
			Model:      mName,
			FeatureSet: "L5",
			Dataset:    datasetPath,
			Horizon:    common.H30s,
			Symbol:     "BTCUSDT",
			Params:     map[string]any{"lambda": 0.01, "alpha": 0.5, "forgetting_factor": 0.98},
			Split:      DefaultSplit(common.H30s),
		}

		if err := Run(context.Background(), exp, cfg, nil); err != nil {
			t.Fatalf("Model %s run failed: %v", mName, err)
		}
		if exp.State != StateCompleted {
			t.Fatalf("Model %s state = %s, want completed", mName, exp.State)
		}
		if len(exp.TestMetrics.EquityCurve) == 0 {
			t.Fatalf("Model %s missing equity curve in TestMetrics", mName)
		}
	}
}

func TestInterruptedRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	expDir := filepath.Join(tmpDir, "experiments")
	_ = os.MkdirAll(expDir, 0o755)

	// Write an experiment marked running as if interrupted by crash
	mockExp := `{"id":"crash-exp","name":"Crashed Job","state":"running","created_at":"2026-11-01T12:00:00Z"}`
	_ = os.WriteFile(filepath.Join(expDir, "crash-exp.json"), []byte(mockExp), 0o644)

	mgr, err := NewManager(tmpDir, &config.Config{})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	rec, ok := mgr.GetExperiment("crash-exp")
	if !ok {
		t.Fatal("crashed experiment not found")
	}
	if rec.State != StateInterrupted {
		t.Fatalf("reopened experiment state = %s, want %s", rec.State, StateInterrupted)
	}
}
