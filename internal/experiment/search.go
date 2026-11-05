package experiment

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/models/common"
	"piquant-live/internal/models/mlofi"
	"piquant-live/internal/telemetry"
)

// SearchTrial represents one evaluation in a parameter search.
type SearchTrial struct {
	TrialID    int            `json:"trial_id"`
	Params     map[string]any `json:"params"`
	FeatureSet string         `json:"feature_set"`
	Lambda     float64        `json:"lambda"`
	ValMAE     float64        `json:"val_mae"`
	ValRMSE    float64        `json:"val_rmse"`
	ValDirAcc  float64        `json:"val_dir_acc"`
	ValRho     float64        `json:"val_rho"`
	DurationMs int64          `json:"duration_ms"`
}

// ParameterSearch holds a hyperparameter exploration experiment.
type ParameterSearch struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Model       string         `json:"model"`
	Dataset     string         `json:"dataset"`
	Horizon     common.Horizon `json:"horizon"`
	Symbol      string         `json:"symbol"`
	State       State          `json:"state"`
	Progress    float64        `json:"progress"`
	Trials      []SearchTrial  `json:"trials"`
	BestTrial   *SearchTrial   `json:"best_trial,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	StartedAt   time.Time      `json:"started_at,omitempty"`
	FinishedAt  time.Time      `json:"finished_at,omitempty"`
	DurationMs  int64          `json:"duration_ms"`
	TestMetrics *Metrics       `json:"test_metrics,omitempty"` // Computed ONCE on the best trial
	Error       string         `json:"error,omitempty"`
}

// SearchSpace defines the parameter grid.
type SearchSpace struct {
	FeatureSets []string  `json:"feature_sets"`
	Lambdas     []float64 `json:"lambdas"`
}

// DefaultSearchSpace provides safe, standard hyperparameter defaults.
func DefaultSearchSpace() SearchSpace {
	return SearchSpace{
		FeatureSets: []string{string(mlofi.SetL1), string(mlofi.SetL5), string(mlofi.SetL20), string(mlofi.SetMulti)},
		Lambdas:     []float64{1e-4, 1e-3, 1e-2, 1e-1, 1.0},
	}
}

// RunSearch executes a parameter search across the search space with high-throughput in-memory caching.
// It extracts dataset observations ONCE and evaluates candidates strictly on validation performance.
// Held-out test performance is evaluated exclusively on the selected candidate.
func RunSearch(ctx context.Context, ps *ParameterSearch, space SearchSpace, cfg *config.Config, onProgress func(pct float64)) error {
	ps.StartedAt = time.Now()
	ps.State = StateRunning
	ps.Progress = 0.05
	if onProgress != nil {
		onProgress(0.05)
	}

	if len(space.FeatureSets) == 0 {
		space.FeatureSets = []string{string(mlofi.SetL5), string(mlofi.SetMulti)}
	}
	if len(space.Lambdas) == 0 {
		space.Lambdas = []float64{1e-3, 1e-2, 1e-1}
	}

	// 1. Efficient single-pass extraction of order book observations and forward horizon returns
	samples, err := ExtractSamples(ctx, ps.Dataset, ps.Horizon, cfg, func(pct float64) {
		if onProgress != nil {
			onProgress(0.05 + pct*0.35)
		}
	})
	if err != nil {
		ps.State = StateFailed
		ps.Error = fmt.Sprintf("dataset extraction failed: %v", err)
		return err
	}

	// 2. Chronological partition into Train / Val / Test with Embargo Gap
	trainSamples, valSamples, testSamples, err := PartitionSamples(samples, DefaultSplit(ps.Horizon))
	if err != nil {
		ps.State = StateFailed
		ps.Error = err.Error()
		return err
	}

	type trialSpec struct {
		trialID int
		fsetStr string
		lambda  float64
	}
	var specs []trialSpec
	trialCounter := 0
	for _, fsetStr := range space.FeatureSets {
		for _, lam := range space.Lambdas {
			trialCounter++
			specs = append(specs, trialSpec{trialID: trialCounter, fsetStr: fsetStr, lambda: lam})
		}
	}
	totalCombinations := len(specs)

	var mu sync.Mutex
	var bestTrial *SearchTrial
	bestScore := math.MaxFloat64 // Lower Val MAE is better
	completedCount := 0

	valActuals := make([]float64, len(valSamples))
	for i, s := range valSamples {
		valActuals[i] = s.Y
	}

	testActuals := make([]float64, len(testSamples))
	for i, s := range testSamples {
		testActuals[i] = s.Y
	}

	trialChan := make(chan trialSpec, totalCombinations)
	for _, sp := range specs {
		trialChan <- sp
	}
	close(trialChan)

	var tempC float64
	if hs := telemetry.New().Collect(); hs.Host.TempOK {
		tempC = float64(hs.Host.TempC)
	}
	_, maxWorkers := ShouldThrottleForThermal(tempC)
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	var wg sync.WaitGroup

	for w := 0; w < maxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sp := range trialChan {
				if ctx.Err() != nil {
					return
				}
				trialStart := time.Now()
				fset := mlofi.FeatureSet(sp.fsetStr)
				featIdx, err := mlofi.Features(fset)
				if err != nil {
					featIdx = []int{0, 1, 2, 3, 4, 5, 6}
				}

				model := InstantiateModel(ps.Model, sp.lambda, map[string]any{"lambda": sp.lambda})
				if err := model.Fit(trainSamples, featIdx); err != nil {
					continue
				}

				valPreds := make([]float64, len(valSamples))
				for i, s := range valSamples {
					valPreds[i] = model.Predict(&s.X)
				}
				valMetrics := ComputeMetrics(valPreds, valActuals)

				trialDur := time.Since(trialStart).Milliseconds()
				trial := SearchTrial{
					TrialID:    sp.trialID,
					Params:     map[string]any{"lambda": sp.lambda, "feature_set": sp.fsetStr},
					FeatureSet: sp.fsetStr,
					Lambda:     sp.lambda,
					ValMAE:     valMetrics.MAE,
					ValRMSE:    valMetrics.RMSE,
					ValDirAcc:  valMetrics.DirectionalAccuracy,
					ValRho:     valMetrics.RankCorrelation,
					DurationMs: trialDur,
				}

				mu.Lock()
				ps.Trials = append(ps.Trials, trial)
				if trial.ValMAE > 0 && trial.ValMAE < bestScore {
					bestScore = trial.ValMAE
					currentTrial := trial
					bestTrial = &currentTrial
				}
				completedCount++
				ps.Progress = 0.40 + 0.50*(float64(completedCount)/float64(totalCombinations))
				prog := ps.Progress
				mu.Unlock()

				if onProgress != nil {
					onProgress(prog)
				}
			}
		}()
	}
	wg.Wait()

	if ctx.Err() != nil {
		ps.State = StateCancelled
		ps.Error = "search cancelled by user"
		return ctx.Err()
	}

	sort.Slice(ps.Trials, func(i, j int) bool {
		return ps.Trials[i].TrialID < ps.Trials[j].TrialID
	})

	if bestTrial == nil {
		ps.State = StateFailed
		ps.Error = "no trials succeeded"
		return fmt.Errorf("search failed: %s", ps.Error)
	}

	ps.BestTrial = bestTrial

	// Phase 3: Final single evaluation of the selected best candidate on held-out test data
	bestFset := mlofi.FeatureSet(bestTrial.FeatureSet)
	bestFeatIdx, err := mlofi.Features(bestFset)
	if err != nil {
		bestFeatIdx = []int{0, 1, 2, 3, 4, 5, 6}
	}

	bestModel := InstantiateModel(ps.Model, bestTrial.Lambda, map[string]any{"lambda": bestTrial.Lambda})
	if err := bestModel.Fit(trainSamples, bestFeatIdx); err == nil {
		testPreds := make([]float64, len(testSamples))
		for i, s := range testSamples {
			testPreds[i] = bestModel.Predict(&s.X)
		}
		testMetrics := ComputeMetrics(testPreds, testActuals)
		ps.TestMetrics = &testMetrics
	}

	ps.FinishedAt = time.Now()
	ps.DurationMs = ps.FinishedAt.Sub(ps.StartedAt).Milliseconds()
	ps.State = StateCompleted
	ps.Progress = 1.0
	if onProgress != nil {
		onProgress(1.0)
	}

	return nil
}
