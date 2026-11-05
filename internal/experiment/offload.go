package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"piquant-live/internal/models/common"
	"piquant-live/internal/models/mlofi"
)

// WorkerEvalRequest is the compact RPC payload sent from Pi 5 to Pi 3B
// during distributed work-stealing hyperparameter search or cross-validation.
type WorkerEvalRequest struct {
	TrialID      int             `json:"trial_id"`
	Model        string          `json:"model"`
	FeatureSet   string          `json:"feature_set"`
	Lambda       float64         `json:"lambda"`
	Params       map[string]any  `json:"params,omitempty"`
	TrainSamples []common.Sample `json:"train_samples"`
	ValSamples   []common.Sample `json:"val_samples"`
}

// WorkerEvalResponse is returned by Pi 3B (or local fallback) after evaluating a trial.
type WorkerEvalResponse struct {
	OK         bool        `json:"ok"`
	WorkerNode string      `json:"worker_node"` // "pi3b" or "pi5"
	Trial      SearchTrial `json:"trial"`
	ValMetrics Metrics     `json:"val_metrics"`
	Error      string      `json:"error,omitempty"`
}

// EvaluateWorkerTrial executes a single candidate trial evaluation in memory.
// Safe to run on both Raspberry Pi 3B (1 GB RAM) and Raspberry Pi 5.
func EvaluateWorkerTrial(req WorkerEvalRequest, nodeName string) WorkerEvalResponse {
	start := time.Now()
	if len(req.TrainSamples) < 10 || len(req.ValSamples) < 5 {
		return WorkerEvalResponse{
			OK:         false,
			WorkerNode: nodeName,
			Error:      "insufficient train/val samples in worker payload",
		}
	}

	fset := mlofi.FeatureSet(req.FeatureSet)
	featIdx, err := mlofi.Features(fset)
	if err != nil {
		featIdx = []int{0, 1, 2, 3, 4, 5, 6}
	}

	lam := req.Lambda
	if lam <= 0 {
		lam = 1e-3
	}
	params := req.Params
	if params == nil {
		params = map[string]any{"lambda": lam}
	}

	model := InstantiateModel(req.Model, lam, params)
	if err := model.Fit(req.TrainSamples, featIdx); err != nil {
		return WorkerEvalResponse{
			OK:         false,
			WorkerNode: nodeName,
			Error:      fmt.Sprintf("fit failed: %v", err),
		}
	}

	valPreds := make([]float64, len(req.ValSamples))
	valActuals := make([]float64, len(req.ValSamples))
	for i, s := range req.ValSamples {
		valPreds[i] = model.Predict(&s.X)
		valActuals[i] = s.Y
	}
	valM := ComputeMetrics(valPreds, valActuals)
	dur := time.Since(start).Milliseconds()

	trial := SearchTrial{
		TrialID:    req.TrialID,
		Params:     map[string]any{"lambda": lam, "feature_set": req.FeatureSet, "worker": nodeName},
		FeatureSet: req.FeatureSet,
		Lambda:     lam,
		ValMAE:     valM.MAE,
		ValRMSE:    valM.RMSE,
		ValDirAcc:  valM.DirectionalAccuracy,
		ValRho:     valM.RankCorrelation,
		DurationMs: dur,
	}

	return WorkerEvalResponse{
		OK:         true,
		WorkerNode: nodeName,
		Trial:      trial,
		ValMetrics: valM,
	}
}

// OffloadTrialToPeer dispatches a candidate evaluation to a discovered Pi 3B worker over HTTP.
// If the peer is unreachable, busy, or times out (>1.5s), it falls back transparently to local Pi 5 execution.
func OffloadTrialToPeer(ctx context.Context, peerAddr, peerToken string, req WorkerEvalRequest) WorkerEvalResponse {
	if peerAddr == "" {
		return EvaluateWorkerTrial(req, "pi5")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return EvaluateWorkerTrial(req, "pi5")
	}

	offCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	url := "http://" + peerAddr + "/api/worker/eval"
	httpReq, err := http.NewRequestWithContext(offCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return EvaluateWorkerTrial(req, "pi5")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if peerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+peerToken)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return EvaluateWorkerTrial(req, "pi5")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return EvaluateWorkerTrial(req, "pi5")
	}

	var workerResp WorkerEvalResponse
	if err := json.NewDecoder(resp.Body).Decode(&workerResp); err != nil || !workerResp.OK {
		return EvaluateWorkerTrial(req, "pi5")
	}
	return workerResp
}
