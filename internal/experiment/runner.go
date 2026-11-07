package experiment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"piquant-live/internal/config"
	"piquant-live/internal/features"
	"piquant-live/internal/models/common"
	"piquant-live/internal/models/mlofi"
	"piquant-live/internal/replay"
)

// FeatureObservation captures features at one point in time and tracks future mid prices.
type FeatureObservation struct {
	Timestamp int64
	MidPrice  float64
	Features  [common.FeatureCount]float64
}

// ContinuousModel is the shared interface implemented by all regression and classification predictors.
type ContinuousModel interface {
	Fit(samples []common.Sample, idx []int) error
	Predict(x *[common.FeatureCount]float64) float64
}

type logisticWrapper struct {
	mod *common.Logistic
}

func (w *logisticWrapper) Fit(samples []common.Sample, idx []int) error {
	binSamples := make([]common.Sample, len(samples))
	for i, s := range samples {
		binSamples[i] = s
		if s.Y > 0 {
			binSamples[i].Y = 1
		} else {
			binSamples[i].Y = 0
		}
	}
	return w.mod.Fit(binSamples, idx)
}

func (w *logisticWrapper) Predict(x *[common.FeatureCount]float64) float64 {
	return w.mod.Logit(x) * 2.0
}

type esnWrapper struct {
	mod *common.ESNReservoirModel
}

func (w *esnWrapper) Fit(samples []common.Sample, idx []int) error {
	return w.mod.Fit(samples, idx)
}

func (w *esnWrapper) Predict(x *[common.FeatureCount]float64) float64 {
	return w.mod.Predict(x)
}

// InstantiateModel creates an unfitted predictor for the specified model family.
func InstantiateModel(modelName string, lambda float64, params map[string]any) ContinuousModel {
	switch modelName {
	case "elastic_net":
		alpha := 0.5
		if a, ok := params["alpha"].(float64); ok && a >= 0 && a <= 1 {
			alpha = a
		}
		return common.NewElasticNet(lambda, alpha)
	case "adaptive_rls":
		ff := 0.99
		if f, ok := params["forgetting_factor"].(float64); ok && f > 0.8 && f <= 1.0 {
			ff = f
		}
		return common.NewAdaptiveRLS(ff)
	case "queue_logistic":
		return &logisticWrapper{mod: common.NewLogistic(lambda)}
	case "meta_labeler":
		feeHurdle := 10.0
		if f, ok := params["fee_hurdle_bps"].(float64); ok && f > 0 {
			feeHurdle = f
		}
		threshold := 0.55
		if t, ok := params["prob_threshold"].(float64); ok && t > 0.5 && t < 1.0 {
			threshold = t
		}
		base := common.NewRidge(lambda)
		return common.NewMetaLabelingFilter(base, feeHurdle, threshold)
	case "glft_maker":
		gamma := 0.05
		if g, ok := params["gamma"].(float64); ok && g > 0 {
			gamma = g
		}
		makerFee := 0.0
		if mf, ok := params["maker_fee_bps"].(float64); ok {
			makerFee = mf
		}
		return common.NewGLFTMarketMaker(lambda, gamma, makerFee)
	case "regime_ensemble":
		return common.NewRegimeSwitchingEnsemble(lambda)
	case "perp_liquidation_reversion":
		return common.NewPerpLiquidationReversionModel(lambda)
	case "rough_signature_ofi":
		return common.NewRoughSignatureOFIModel(lambda)
	case "cartea_jaimungal":
		gamma := 0.05
		if g, ok := params["gamma"].(float64); ok && g > 0 {
			gamma = g
		}
		return common.NewCarteaJaimungalMM(gamma, 1.5, 2.0, 0.001, 0.01, 0.5, 60.0)
	case "esn_reservoir":
		return &esnWrapper{mod: common.NewESNReservoirModel(lambda)}
	case "spot_perp_basis":
		theta := 0.65
		if th, ok := params["theta"].(float64); ok && th > 0 {
			theta = th
		}
		zThresh := 1.5
		if z, ok := params["z_threshold"].(float64); ok && z > 0 {
			zThresh = z
		}
		return common.NewSpotPerpBasisModel(lambda, theta, zThresh)
	case "triangular_arb":
		feeA := 2.0
		feeB := 2.0
		feeC := 2.0
		thresh := 0.5
		if fa, ok := params["fee_a_bps"].(float64); ok && fa >= 0 {
			feeA = fa
		}
		if fb, ok := params["fee_b_bps"].(float64); ok && fb >= 0 {
			feeB = fb
		}
		if fc, ok := params["fee_c_bps"].(float64); ok && fc >= 0 {
			feeC = fc
		}
		if th, ok := params["threshold_bps"].(float64); ok && th >= 0 {
			thresh = th
		}
		return common.NewTriangularArbModel(lambda, feeA, feeB, feeC, thresh)
	case "cross_venue_leadlag":
		venueA := "Binance"
		venueB := "Bybit"
		if va, ok := params["venue_a"].(string); ok && va != "" {
			venueA = va
		}
		if vb, ok := params["venue_b"].(string); ok && vb != "" {
			venueB = vb
		}
		return common.NewCrossVenueLeadLagModel(lambda, venueA, venueB)
	case "vpin_toxicity":
		threshold := 0.70
		if t, ok := params["threshold"].(float64); ok && t > 0 {
			threshold = t
		}
		return common.NewVPINToxicityModel(lambda, threshold)
	case "kyles_lambda":
		alpha := 0.05
		if a, ok := params["alpha"].(float64); ok && a > 0 {
			alpha = a
		}
		return common.NewKylesLambdaModel(lambda, alpha)
	case "adaptive_meta_ensemble":
		gamma := 0.98
		if g, ok := params["gamma"].(float64); ok && g > 0 && g <= 1.0 {
			gamma = g
		}
		eta := 0.15
		if e, ok := params["eta"].(float64); ok && e > 0 {
			eta = e
		}
		return common.NewAdaptiveMetaEnsemble(lambda, gamma, eta)
	case "funding_carry":
		return common.NewFundingCarryModel(lambda)
	default:
		return &common.Ridge{Lam: lambda}
	}
}

type sampleCacheEntry struct {
	modTime time.Time
	size    int64
	samples []common.Sample
}

var (
	samplesCacheMu sync.RWMutex
	samplesCache   = make(map[string]sampleCacheEntry)
)

// ExtractSamples deterministically reads the dataset, calculates orderbook features, and computes forward horizon returns.
// Utilizes an in-memory hot-path cache and an on-disk binary .pqbin cache (<datasetPath>.<horizonNs>.pqbin)
// to ensure subsequent sweeps and backtests load in <1ms without JSON parsing.
func ExtractSamples(ctx context.Context, datasetPath string, horizon common.Horizon, cfg *config.Config, onProgress func(pct float64)) ([]common.Sample, error) {
	horizonNs := horizon.Duration().Nanoseconds()
	if horizonNs <= 0 {
		horizonNs = 30 * time.Second.Nanoseconds()
	}
	pqbinPath := fmt.Sprintf("%s.%d.pqbin", datasetPath, horizonNs)

	info, statErr := os.Stat(datasetPath)
	cacheKey := fmt.Sprintf("%s:%s", datasetPath, horizon.String())
	if statErr == nil {
		samplesCacheMu.RLock()
		cached, ok := samplesCache[cacheKey]
		samplesCacheMu.RUnlock()
		if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
			if onProgress != nil {
				onProgress(1.0)
			}
			res := make([]common.Sample, len(cached.samples))
			copy(res, cached.samples)
			return res, nil
		}

		// Fast binary columnar cache check (<1ms without JSON parsing)
		if binSamples, ok := LoadBinarySampleCache(pqbinPath, info.ModTime().UnixNano()); ok && len(binSamples) >= 30 {
			samplesCacheMu.Lock()
			samplesCache[cacheKey] = sampleCacheEntry{
				modTime: info.ModTime(),
				size:    info.Size(),
				samples: binSamples,
			}
			samplesCacheMu.Unlock()
			if onProgress != nil {
				onProgress(1.0)
			}
			res := make([]common.Sample, len(binSamples))
			copy(res, binSamples)
			return res, nil
		}
	}

	if cfg == nil {
		cfg = &config.Config{
			Symbols: []config.Symbol{
				{Name: "BTC", VenueSym: "BTCUSDT", Venue: "binance"},
				{Name: "ETH", VenueSym: "ETHUSDT", Venue: "binance"},
			},
			Targets: map[string][]string{
				"BTC": {"binance:BTCUSDT"},
				"ETH": {"binance:ETHUSDT"},
			},
		}
	}
	rep, err := replay.Open(datasetPath, replay.Options{
		Config:   cfg,
		Speed:    0, // max speed
		MaxBooks: 16,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot open dataset %s: %v", datasetPath, err)
	}

	featEng := features.New(0)
	obs := make([]FeatureObservation, 0, 4096)
	featDst := make([]float32, features.NCore)

	count := 0
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		step, err := rep.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, replay.ErrFinished) {
			break
		}
		if err != nil {
			continue // skip corrupted line
		}

		b := step.Book
		if b != nil && b.Ready {
			featEng.Advance(b, &step.Env)
			featEng.Extract(b, featDst)

			// Sample every 5 events or when mid changes
			midInt, ok := b.Mid()
			if ok && midInt > 0 && (count%5 == 0) {
				var f64 [common.FeatureCount]float64
				for i := 0; i < common.FeatureCount && i < len(featDst); i++ {
					f64[i] = float64(featDst[i])
				}
				obs = append(obs, FeatureObservation{
					Timestamp: step.Env.RecvTime,
					MidPrice:  float64(midInt),
					Features:  f64,
				})
			}
		}
		count++
		if count%500 == 0 && onProgress != nil {
			onProgress(math.Min(1.0, float64(count)*0.0002))
		}
	}

	if len(obs) < 50 {
		return nil, fmt.Errorf("insufficient valid book observations (%d found, minimum 50 required)", len(obs))
	}

	samples := make([]common.Sample, 0, len(obs))
	nObs := len(obs)
	targetIdx := 0

	for i := 0; i < nObs; i++ {
		targetTime := obs[i].Timestamp + horizonNs
		for targetIdx < nObs && obs[targetIdx].Timestamp < targetTime {
			targetIdx++
		}
		if targetIdx >= nObs {
			break
		}

		futureMid := obs[targetIdx].MidPrice
		currentMid := obs[i].MidPrice
		if currentMid > 0 && futureMid > 0 {
			yBps := common.Bps(currentMid, futureMid)
			// Consider rapid market drops in BTC/ETH and stablecoin basis shocks:
			// During sharp drops (negative momentum) or wide spread, apply asymmetric liquidation slippage
			spreadBps := obs[i].Features[4]
			ret1s := obs[i].Features[11]
			if ret1s < -8.0 || spreadBps > 4.0 {
				if yBps < 0 {
					yBps -= 0.10 // liquidation cascade bid depletion
				}
			}
			targetCls := int64(0)
			if yBps > 0 {
				targetCls = 1
			} else if yBps < 0 {
				targetCls = -1
			}
			samples = append(samples, common.Sample{
				X:         obs[i].Features,
				Y:         yBps,
				TsNs:      obs[i].Timestamp,
				Mid:       currentMid,
				Spread:    spreadBps,
				Vol:       obs[i].Features[10],
				TargetCls: targetCls,
			})
		}
	}

	if len(samples) < 30 {
		return nil, fmt.Errorf("insufficient forward labeled samples (%d found, minimum 30 required)", len(samples))
	}

	if statErr == nil && len(samples) > 0 {
		_ = SaveBinarySampleCacheWithMtime(pqbinPath, info.ModTime().UnixNano(), samples)
		samplesCacheMu.Lock()
		samplesCache[cacheKey] = sampleCacheEntry{
			modTime: info.ModTime(),
			size:    info.Size(),
			samples: samples,
		}
		samplesCacheMu.Unlock()
	}

	return samples, nil
}

// PartitionSamples splits samples chronologically into train, val, and test slices with an embargo buffer.
func PartitionSamples(samples []common.Sample, split SplitConfig) (train, val, test []common.Sample, err error) {
	nTotal := len(samples)
	if nTotal < 30 {
		return nil, nil, nil, fmt.Errorf("insufficient forward labeled samples (%d found, minimum 30 required)", nTotal)
	}

	embargoGap := int(math.Ceil(float64(nTotal) * 0.02))
	if embargoGap < 2 {
		embargoGap = 2
	}

	trainPct := split.TrainPct
	valPct := split.ValPct
	if trainPct <= 0 || valPct <= 0 {
		trainPct = 0.60
		valPct = 0.20
	}

	nTrain := int(float64(nTotal) * trainPct)
	nVal := int(float64(nTotal) * valPct)

	trainEnd := nTrain
	valStart := trainEnd + embargoGap
	valEnd := valStart + nVal
	testStart := valEnd + embargoGap
	testEnd := nTotal

	if testStart >= testEnd || valStart >= valEnd {
		// Fallback without gap if dataset is compact
		trainEnd = int(float64(nTotal) * 0.60)
		valStart = trainEnd
		valEnd = valStart + int(float64(nTotal)*0.20)
		testStart = valEnd
		testEnd = nTotal
	}

	return samples[0:trainEnd], samples[valStart:valEnd], samples[testStart:testEnd], nil
}

// Run executes an experiment deterministically and updates its fields.
func Run(ctx context.Context, exp *Experiment, cfg *config.Config, onProgress func(stage string, pct float64)) error {
	exp.StartedAt = time.Now()
	exp.State = StateRunning
	exp.Stage = "replay"
	exp.Progress = 0.05
	if onProgress != nil {
		onProgress(exp.Stage, exp.Progress)
	}

	samples, err := ExtractSamples(ctx, exp.Dataset, exp.Horizon, cfg, func(pct float64) {
		if onProgress != nil {
			onProgress("replay", 0.05+pct*0.40)
		}
	})
	if err != nil {
		exp.State = StateFailed
		exp.Error = err.Error()
		return err
	}

	exp.Stage = "partitioning"
	exp.Progress = 0.50
	if onProgress != nil {
		onProgress(exp.Stage, exp.Progress)
	}

	trainSamples, valSamples, testSamples, err := PartitionSamples(samples, exp.Split)
	if err != nil {
		exp.State = StateFailed
		exp.Error = err.Error()
		return err
	}

	exp.Stage = "fitting"
	exp.Progress = 0.65
	if onProgress != nil {
		onProgress(exp.Stage, exp.Progress)
	}

	// Parse feature set
	fset := mlofi.FeatureSet(exp.FeatureSet)
	if fset == "" {
		fset = mlofi.SetMulti
	}
	featIdx, err := mlofi.Features(fset)
	if err != nil {
		featIdx = []int{0, 1, 2, 3, 4, 5, 6}
	}

	lambda := 1e-3
	if l, ok := exp.Params["lambda"].(float64); ok && l > 0 {
		lambda = l
	}

	model := InstantiateModel(exp.Model, lambda, exp.Params)
	if err := model.Fit(trainSamples, featIdx); err != nil {
		exp.State = StateFailed
		exp.Error = fmt.Sprintf("model fit failed: %v", err)
		return err
	}

	exp.Stage = "evaluating"
	exp.Progress = 0.85
	if onProgress != nil {
		onProgress(exp.Stage, exp.Progress)
	}

	// Predict function
	evalSplit := func(split []common.Sample) ([]float64, []float64) {
		preds := make([]float64, len(split))
		actuals := make([]float64, len(split))
		for i, s := range split {
			preds[i] = model.Predict(&s.X)
			actuals[i] = s.Y
		}
		return preds, actuals
	}

	trainPred, _ := evalSplit(trainSamples)
	valPred, _ := evalSplit(valSamples)
	testPred, _ := evalSplit(testSamples)

	exp.TrainMetrics = EvaluatePredictions(trainSamples, trainPred)
	exp.ValMetrics = EvaluatePredictions(valSamples, valPred)
	exp.TestMetrics = EvaluatePredictions(testSamples, testPred)

	// Purged K-Fold Cross-Validation & Probability of Backtest Overfitting (PBO)
	cvRes := RunPurgedKFoldCV(append(append([]common.Sample{}, trainSamples...), valSamples...), exp.Model, lambda, exp.Params, featIdx, 5)
	exp.ValMetrics.PurgedCVMAE = cvRes.MeanMAE
	exp.ValMetrics.ProbBacktestOverfit = cvRes.ProbBacktestOverfit
	exp.TestMetrics.PurgedCVMAE = cvRes.MeanMAE
	exp.TestMetrics.ProbBacktestOverfit = cvRes.ProbBacktestOverfit

	// Rolling Walk-Forward Validation (Temporal IC & Stability Ratio)
	wfIC, wfStab, _ := RunWalkForwardValidation(samples, 4, func(wfTrain []common.Sample) func([]float64) float64 {
		wfMod := InstantiateModel(exp.Model, lambda, exp.Params)
		if err := wfMod.Fit(wfTrain, featIdx); err != nil {
			return nil
		}
		return func(f []float64) float64 {
			var x [common.FeatureCount]float64
			copy(x[:], f)
			return wfMod.Predict(&x)
		}
	})
	exp.ValMetrics.WalkForwardIC = wfIC
	exp.ValMetrics.WalkForwardStability = wfStab
	exp.TestMetrics.WalkForwardIC = wfIC
	exp.TestMetrics.WalkForwardStability = wfStab

	// Queue-reactive maker fill & adverse selection simulation
	mmSim := common.NewGLFTMarketMaker(lambda, 0.05, 0.0)
	_ = mmSim.Fit(trainSamples, featIdx)
	valQStats := common.SimulateQueueMakerFills(valSamples, mmSim)
	testQStats := common.SimulateQueueMakerFills(testSamples, mmSim)
	exp.ValMetrics.MakerFillRate = valQStats.FillRate
	exp.ValMetrics.AdverseSelectionBps = valQStats.AdverseSelectionBps
	exp.TestMetrics.MakerFillRate = testQStats.FillRate
	exp.TestMetrics.AdverseSelectionBps = testQStats.AdverseSelectionBps

	exp.OverfitWarning = CheckGeneralization(exp.TrainMetrics, exp.ValMetrics, exp.TestMetrics)

	exp.FinishedAt = time.Now()
	exp.DurationMs = exp.FinishedAt.Sub(exp.StartedAt).Milliseconds()
	exp.State = StateCompleted
	exp.Stage = "completed"
	exp.Progress = 1.0
	if onProgress != nil {
		onProgress(exp.Stage, exp.Progress)
	}

	return nil
}
