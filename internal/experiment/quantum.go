package experiment

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/models/mlofi"
)

// QuantumSearchOptions configures the Quantum-Inspired Simulated Annealing optimizer.
type QuantumSearchOptions struct {
	Steps            int     // Total annealing time steps (e.g. 30)
	GammaInitial     float64 // Initial transverse tunneling field amplitude (e.g. 2.5)
	TemperatureStart float64 // Initial thermal temperature (e.g. 1.2)
	TemperatureMin   float64 // Minimum final temperature (e.g. 0.02)
	ParsimonyWeight  float64 // Penalty for adding extra features (promotes sparsity)
	Seed             int64
}

// DefaultQuantumSearchOptions provides tuned quantum annealing parameters.
func DefaultQuantumSearchOptions() QuantumSearchOptions {
	return QuantumSearchOptions{
		Steps:            35,
		GammaInitial:     2.5,
		TemperatureStart: 1.2,
		TemperatureMin:   0.02,
		ParsimonyWeight:  0.02,
		Seed:             42,
	}
}

// RunQuantumSearch performs Quantum-Inspired Annealing with Transverse-Field Tunneling
// to find the optimal hyperparameter vector and feature subset, escaping local minima.
// Evaluates strictly on validation data, touching held-out test data only once for the winning candidate.
func RunQuantumSearch(ctx context.Context, ps *ParameterSearch, opts QuantumSearchOptions, cfg *config.Config, onProgress func(pct float64)) error {
	ps.StartedAt = time.Now()
	ps.State = StateRunning
	ps.Progress = 0.05
	if onProgress != nil {
		onProgress(0.05)
	}

	if opts.Steps <= 0 {
		opts = DefaultQuantumSearchOptions()
	}

	// 1. Efficient single-pass extraction of order book observations
	samples, err := ExtractSamples(ctx, ps.Dataset, ps.Horizon, cfg, func(pct float64) {
		if onProgress != nil {
			onProgress(0.05 + pct*0.30)
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

	valActuals := make([]float64, len(valSamples))
	for i, s := range valSamples {
		valActuals[i] = s.Y
	}
	testActuals := make([]float64, len(testSamples))
	for i, s := range testSamples {
		testActuals[i] = s.Y
	}

	rng := rand.New(rand.NewSource(opts.Seed))
	if opts.Seed == 0 {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}

	featureOptions := []string{string(mlofi.SetL1), string(mlofi.SetL5), string(mlofi.SetL20), string(mlofi.SetMulti)}
	lambdas := []float64{1e-4, 5e-4, 1e-3, 5e-3, 1e-2, 5e-2, 1e-1, 5e-1, 1.0}

	// Current quantum state
	currentFset := featureOptions[rng.Intn(len(featureOptions))]
	currentLamIdx := rng.Intn(len(lambdas))
	currentLam := lambdas[currentLamIdx]

	evalCandidate := func(fsetStr string, lam float64) (*SearchTrial, float64) {
		t0 := time.Now()
		fset := mlofi.FeatureSet(fsetStr)
		featIdx, err := mlofi.Features(fset)
		if err != nil {
			featIdx = []int{0, 1, 2, 3, 4, 5, 6}
		}

		model := InstantiateModel(ps.Model, lam, map[string]any{"lambda": lam})
		if err := model.Fit(trainSamples, featIdx); err != nil {
			return nil, math.MaxFloat64
		}

		valPreds := make([]float64, len(valSamples))
		for i, s := range valSamples {
			valPreds[i] = model.Predict(&s.X)
		}
		valMetrics := ComputeMetrics(valPreds, valActuals)

		trial := &SearchTrial{
			Params:     map[string]any{"lambda": lam, "feature_set": fsetStr, "optimizer": "quantum_annealing"},
			FeatureSet: fsetStr,
			Lambda:     lam,
			ValMAE:     valMetrics.MAE,
			ValRMSE:    valMetrics.RMSE,
			ValDirAcc:  valMetrics.DirectionalAccuracy,
			ValRho:     valMetrics.RankCorrelation,
			DurationMs: time.Since(t0).Milliseconds(),
		}

		// Objective energy: Val MAE + parsimony penalty for feature count
		energy := valMetrics.MAE + float64(len(featIdx))*opts.ParsimonyWeight
		return trial, energy
	}

	initialTrial, currentEnergy := evalCandidate(currentFset, currentLam)
	if initialTrial != nil {
		initialTrial.TrialID = 1
		ps.Trials = append(ps.Trials, *initialTrial)
	}

	var bestTrial *SearchTrial
	bestEnergy := currentEnergy
	if initialTrial != nil {
		bestTrial = initialTrial
	}

	// Quantum Simulated Annealing Loop with Transverse Field Tunneling
	for step := 1; step <= opts.Steps; step++ {
		if ctx.Err() != nil {
			ps.State = StateCancelled
			ps.Error = "quantum search cancelled by user"
			return ctx.Err()
		}

		// Linear decay of transverse tunneling field Gamma(t)
		progress := float64(step) / float64(opts.Steps)
		gamma := opts.GammaInitial * (1.0 - progress)
		// Geometric cooling of thermal temperature
		temp := opts.TemperatureStart * math.Pow(opts.TemperatureMin/opts.TemperatureStart, progress)

		// Propose neighbor state (quantum fluctuation / spin flip)
		candidateFset := currentFset
		candidateLamIdx := currentLamIdx

		if rng.Float64() < 0.5 {
			// Perturb feature subset
			candidateFset = featureOptions[rng.Intn(len(featureOptions))]
		} else {
			// Perturb regularization scale
			delta := rng.Intn(3) - 1
			candidateLamIdx = currentLamIdx + delta
			if candidateLamIdx < 0 {
				candidateLamIdx = 0
			}
			if candidateLamIdx >= len(lambdas) {
				candidateLamIdx = len(lambdas) - 1
			}
		}
		candidateLam := lambdas[candidateLamIdx]

		trial, candidateEnergy := evalCandidate(candidateFset, candidateLam)
		if trial == nil {
			continue
		}
		trial.TrialID = len(ps.Trials) + 1
		ps.Trials = append(ps.Trials, *trial)

		deltaE := candidateEnergy - currentEnergy

		// Quantum tunneling acceptance probability:
		// P = exp(-deltaE / T + gamma / (T + 1e-4))
		accept := false
		if deltaE <= 0 {
			accept = true
		} else {
			// Transverse tunneling term allows state to tunnel through thin noise barriers
			tunnelingBonus := gamma / (temp*2.0 + 1e-3)
			pAccept := math.Exp(-deltaE/temp + math.Min(tunnelingBonus, 2.0))
			if rng.Float64() < pAccept {
				accept = true
			}
		}

		if accept {
			currentFset = candidateFset
			currentLamIdx = candidateLamIdx
			currentLam = candidateLam
			currentEnergy = candidateEnergy

			if candidateEnergy < bestEnergy {
				bestEnergy = candidateEnergy
				tCopy := *trial
				bestTrial = &tCopy
			}
		}

		ps.Progress = 0.35 + 0.60*progress
		if onProgress != nil {
			onProgress(ps.Progress)
		}
	}

	if bestTrial == nil {
		ps.State = StateFailed
		ps.Error = "no quantum trials succeeded"
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
